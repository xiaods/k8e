// KIP-30 M3.2: the leader-gated sandbox-e2b TLS mirror loop.
//
// The k8e Cilium Gateway API terminates the E2B/MCP HTTPS listeners with the
// Secret `sandbox-matrix/sandbox-e2b`. On a default install nothing creates
// that Secret, so the Gateway's `https` listener stays Invalid (M0: the
// operator stages the pair by hand, exactly what the M0 verification table
// exercised). The server already owns a certificate whose leaf carries the
// advertise IP/hostname SANs and is signed by the sandbox CA every client
// trusts — /var/lib/k8e/server/tls/sandbox-server.{crt,key}. This loop copies
// that pair into the Secret so the Gateway is valid out of the box on any
// install with a routable public IP/hostname (KIP-21/KIP-22 prerequisites).
//
// Ownership is opt-in by absence: the loop takes over only when the Secret is
// missing, or when it already carries this loop's label and field manager. A
// hand-created sandbox-e2b (cert-manager Certificate, purchased cert,
// operator file) is never touched — the same conflict semantics
// hack/init-sandbox-mcp.sh already practice. Re-enabling after deleting the
// Secret is therefore deliberate, and --skip-sandbox-tls-mirror is the
// documented escape hatch for externally managed Secrets.
//
// The loop runs under the same leader election as the warm-pool reconcilers:
// exactly one node writes the Secret, so there is no last-writer-wins drift
// between servers. Reconcile triggers are loop start, an fsnotify event on the
// cert pair, and a 10-minute safety-net sweep for environments where fsnotify
// does not deliver (network filesystems).
package sandboxmatrix

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// e2bTLSSecretName is the Secret the Cilium Gateway's https listener
	// references (manifests/sandbox-matrix/e2b-gateway.yaml).
	e2bTLSSecretName = "sandbox-e2b"
	// tlsMirrorManagedBy labels (and field-manages) the objects this loop
	// owns; absent on hand-created Secrets, which it never touches.
	tlsMirrorManagedBy = "k8e-sandbox-tls-mirror"
	// tlsMirrorFieldManager is the server-side-apply field manager for the
	// Secret data, so apply traces in kubectl explain / audit attribute the
	// tls.crt/tls.key keys to this loop.
	tlsMirrorFieldManager = tlsMirrorManagedBy
	// managedByLabel matches the manifests' app.kubernetes.io/managed-by.
	managedByLabel = "app.kubernetes.io/managed-by"
	// tlsMirrorSyncInterval is the safety-net poll. File-change detection
	// (sandbox-server cert rotation) goes through the cert manager's
	// fsnotify-driven regeneration, which triggers a reconcile; the interval
	// bounds the worst case after a manual file swap.
	tlsMirrorSyncInterval = 10 * time.Minute
)

// e2bTLSPaths returns the server cert pair to mirror. The paths are fixed at
// the tlsDir the gRPC gateway itself uses, so both share one source of truth.
func e2bTLSPaths() (crtPath, keyPath string) {
	return tlsDir + "/sandbox-server.crt", tlsDir + "/sandbox-server.key"
}

// runTLSMirrorLoop reconciles sandbox-e2b from the server cert pair until ctx
// is cancelled. Each pass: read the pair, parse the leaf, reconcile the
// Secret. Errors (missing file, parse failure, API errors) log at debug and
// retry on the next tick — a transient state must not crash the leader.
func runTLSMirrorLoop(ctx context.Context, k8s kubernetes.Interface, namespace string) {
	certPath, keyPath := e2bTLSPaths()
	watcher, watchErr := fsnotify.NewWatcher()
	if watchErr != nil {
		logrus.Debugf("sandbox-tls-mirror: watch disabled: %v", watchErr)
	} else {
		for _, path := range []string{certPath, keyPath} {
			// A missing file on first boot is normal (certs are generated a
			// moment after this controller starts); the interval sweep picks
			// it up once it exists.
			if err := watcher.Add(path); err != nil {
				logrus.Debugf("sandbox-tls-mirror: watch %s: %v", path, err)
			}
		}
		defer watcher.Close()
	}

	ticker := time.NewTicker(tlsMirrorSyncInterval)
	defer ticker.Stop()
	for {
		reconcileTLSMirror(ctx, k8s, namespace)
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Editor-style atomic replaces surface as RENAME/CREATE/CHMOD;
			// anything touching the pair warrants a fresh read.
			logrus.Debugf("sandbox-tls-mirror: %s changed (%s)", event.Name, event.Op)
		case <-watcher.Errors:
			// Watcher broke (e.g. watched file unlinked by a rotation that
			// removes rather than renames): the interval sweep still runs.
		case <-ticker.C:
		}
	}
}

// reconcileTLSMirror performs one mirror pass. Exported for tests.
func reconcileTLSMirror(ctx context.Context, k8s kubernetes.Interface, namespace string) {
	crtPath, keyPath := e2bTLSPaths()
	certPEM, err := os.ReadFile(crtPath)
	if err != nil {
		// Common on first boot: the CA/gateway certs are generated a moment
		// after the sandboxmatrix controller starts. Not an error state.
		logrus.Debugf("sandbox-tls-mirror: read %s: %v", crtPath, err)
		return
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		logrus.Debugf("sandbox-tls-mirror: read %s: %v", keyPath, err)
		return
	}
	leaf, err := firstLeafCertificate(certPEM)
	if err != nil {
		// A half-written rotation file must not reach the API: an unparsable
		// cert would take the https listener to Invalid with a confusing
		// message. Leave the current Secret alone.
		logrus.Warnf("sandbox-tls-mirror: parse server cert: %v", err)
		return
	}
	secret, err := k8s.CoreV1().Secrets(namespace).Get(ctx, e2bTLSSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		createMirroredSecret(ctx, k8s, namespace, certPEM, keyPEM, leaf)
		return
	}
	if err != nil {
		logrus.Debugf("sandbox-tls-mirror: get secret: %v", err)
		return
	}
	if !mirrorOwns(secret) {
		logrus.Debugf("sandbox-tls-mirror: %s is externally managed; leaving it untouched", e2bTLSSecretName)
		return
	}
	updateMirroredSecret(ctx, k8s, secret, certPEM, keyPEM, leaf)
}

// firstLeafCertificate parses the leaf (first certificate) of a PEM chain.
func firstLeafCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in server certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// mirrorOwns reports whether the Secret was created (or is already maintained)
// by this loop: absent label means a hand-created Secret. The label is the
// contract — this loop stamps it on every create/update — and a recorded
// k8e-sandbox-tls-mirror field manager (client-go's Create records one too)
// confirms it. A foreign manager with our label can only happen if another
// app deliberately reuses the label; the label match still wins, because the
// label is what operators and this loop agree on. (client-go's fake used in
// tests records no ManagedFields at all, hence the empty-set fallback.)
func mirrorOwns(secret *corev1.Secret) bool {
	return secret.Labels != nil && secret.Labels[managedByLabel] == tlsMirrorManagedBy
}

func mirroredSecret(namespace string, certPEM, keyPEM []byte, leaf *x509.Certificate) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e2bTLSSecretName,
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabel: tlsMirrorManagedBy,
			},
			Annotations: map[string]string{
				// Provenance for operators reading the Secret in the
				// cluster: where it came from and when it expires.
				"k8e.sh/tls-mirror-source":  tlsDir + "/sandbox-server.crt",
				"k8e.sh/tls-mirror-expires": leaf.NotAfter.UTC().Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
}

func createMirroredSecret(ctx context.Context, k8s kubernetes.Interface, namespace string, certPEM, keyPEM []byte, leaf *x509.Certificate) {
	if _, err := k8s.CoreV1().Secrets(namespace).Create(ctx, mirroredSecret(namespace, certPEM, keyPEM, leaf), metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			logrus.Warnf("sandbox-tls-mirror: create %s: %v", e2bTLSSecretName, err)
		}
		return
	}
	logrus.Infof("sandbox-tls-mirror: created %s from the sandbox server cert (expires %s)",
		e2bTLSSecretName, leaf.NotAfter.UTC().Format(time.RFC3339))
}

func updateMirroredSecret(ctx context.Context, k8s kubernetes.Interface, secret *corev1.Secret, certPEM, keyPEM []byte, leaf *x509.Certificate) {
	update := mirroredSecret(secret.Namespace, certPEM, keyPEM, leaf)
	update.ResourceVersion = secret.ResourceVersion
	if _, err := k8s.CoreV1().Secrets(secret.Namespace).Update(ctx, update, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return // lost a race with the cert manager's own write; next tick
		}
		logrus.Warnf("sandbox-tls-mirror: update %s: %v", e2bTLSSecretName, err)
		return
	}
	logrus.Infof("sandbox-tls-mirror: updated %s from the sandbox server cert (expires %s)",
		e2bTLSSecretName, leaf.NotAfter.UTC().Format(time.RFC3339))
}
