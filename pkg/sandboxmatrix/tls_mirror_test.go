package sandboxmatrix

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// tlsMirrorFixture writes a CA + leaf pair under dir and returns (certPath,
// keyPath, leaf). The leaf carries a known NotAfter so tests can assert the
// expiry annotation.
func tlsMirrorFixture(t *testing.T, dir, name string, notAfter time.Time) (string, string, *x509.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sandbox-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "sandbox-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"sandbox.example.com"},
		IPAddresses:  []net.IP{net.ParseIP("203.0.113.5")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, leaf
}

// withTLSDir repoints e2bTLSPaths at a test directory for one test. tlsDir is
// a package var solely so this works; production never reassigns it.
func withTLSDir(t *testing.T, dir string) {
	t.Helper()
	original := tlsDir
	tlsDir = dir
	t.Cleanup(func() { tlsDir = original })
}

func TestReconcileTLSMirrorCreatesSecretWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	withTLSDir(t, dir)
	certPEM, keyPEM, leaf := tlsMirrorFixture(t, dir, "sandbox-server", time.Now().Add(90*24*time.Hour))
	_ = certPEM
	_ = keyPEM
	k8s := fake.NewSimpleClientset()

	reconcileTLSMirror(context.Background(), k8s, "sandbox-matrix")

	secret, err := k8s.CoreV1().Secrets("sandbox-matrix").Get(context.Background(), e2bTLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("mirror did not create the Secret: %v", err)
	}
	if secret.Labels[managedByLabel] != tlsMirrorManagedBy {
		t.Fatalf("Secret missing ownership label: %v", secret.Labels)
	}
	if secret.Type != corev1.SecretTypeTLS {
		t.Fatalf("Secret type: %s", secret.Type)
	}
	if string(secret.Data[corev1.TLSCertKey]) != string(mustReadFile(t, certPEM)) {
		t.Fatal("mirrored tls.crt does not match the server cert file")
	}
	if string(secret.Data[corev1.TLSPrivateKeyKey]) != string(mustReadFile(t, keyPEM)) {
		t.Fatal("mirrored tls.key does not match the server key file")
	}
	wantExpiry := leaf.NotAfter.UTC().Format(time.RFC3339)
	if secret.Annotations["k8e.sh/tls-mirror-expires"] != wantExpiry {
		t.Fatalf("expiry annotation %q, want %q", secret.Annotations["k8e.sh/tls-mirror-expires"], wantExpiry)
	}
}

func TestReconcileTLSMirrorUpdatesOwnedSecret(t *testing.T) {
	dir := t.TempDir()
	withTLSDir(t, dir)
	// First pair: the loop creates the Secret.
	tlsMirrorFixture(t, dir, "sandbox-server", time.Now().Add(90*24*time.Hour))
	k8s := fake.NewSimpleClientset()
	reconcileTLSMirror(context.Background(), k8s, "sandbox-matrix")
	before, err := k8s.CoreV1().Secrets("sandbox-matrix").Get(context.Background(), e2bTLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Rotation: new leaf (different NotAfter) replaces the pair.
	_, _, newLeaf := tlsMirrorFixture(t, dir, "sandbox-server", time.Now().Add(45*24*time.Hour))
	reconcileTLSMirror(context.Background(), k8s, "sandbox-matrix")

	after, err := k8s.CoreV1().Secrets("sandbox-matrix").Get(context.Background(), e2bTLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := newLeaf.NotAfter.UTC().Format(time.RFC3339)
	if after.Annotations["k8e.sh/tls-mirror-expires"] != want {
		t.Fatalf("expiry annotation %q, want %q", after.Annotations["k8e.sh/tls-mirror-expires"], want)
	}
	if after.Annotations["k8e.sh/tls-mirror-expires"] == before.Annotations["k8e.sh/tls-mirror-expires"] {
		t.Fatal("rotation did not reach the Secret")
	}
}

func TestReconcileTLSMirrorNeverTouchesHandCreatedSecret(t *testing.T) {
	dir := t.TempDir()
	withTLSDir(t, dir)
	tlsMirrorFixture(t, dir, "sandbox-server", time.Now().Add(90*24*time.Hour))
	k8s := fake.NewSimpleClientset()
	hand := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e2bTLSSecretName,
			Namespace: "sandbox-matrix",
			Labels:    map[string]string{managedByLabel: "cert-manager"},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("hand-made-cert"),
			corev1.TLSPrivateKeyKey: []byte("hand-made-key"),
		},
	}
	if _, err := k8s.CoreV1().Secrets("sandbox-matrix").Create(context.Background(), hand, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	reconcileTLSMirror(context.Background(), k8s, "sandbox-matrix")

	secret, err := k8s.CoreV1().Secrets("sandbox-matrix").Get(context.Background(), e2bTLSSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[corev1.TLSCertKey]) != "hand-made-cert" {
		t.Fatal("mirror overwrote a hand-created Secret")
	}
	if _, ok := secret.Annotations["k8e.sh/tls-mirror-expires"]; ok {
		t.Fatal("mirror annotated a Secret it does not own")
	}
}

func TestReconcileTLSMirrorSkipsUnreadableOrUnparsablePair(t *testing.T) {
	dir := t.TempDir()
	withTLSDir(t, dir)
	// Missing key file: reconcile must be a no-op, not a crash.
	tlsMirrorFixture(t, dir, "sandbox-server", time.Now().Add(90*24*time.Hour))
	os.Remove(filepath.Join(dir, "sandbox-server.key"))
	k8s := fake.NewSimpleClientset()
	reconcileTLSMirror(context.Background(), k8s, "sandbox-matrix")
	if _, err := k8s.CoreV1().Secrets("sandbox-matrix").Get(context.Background(), e2bTLSSecretName, metav1.GetOptions{}); err == nil {
		t.Fatal("half-written pair reached the API")
	}

	// Unparsable cert: same — no Secret is created.
	if err := os.WriteFile(filepath.Join(dir, "sandbox-server.key"), []byte("k"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox-server.crt"), []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	reconcileTLSMirror(context.Background(), k8s, "sandbox-matrix")
	if _, err := k8s.CoreV1().Secrets("sandbox-matrix").Get(context.Background(), e2bTLSSecretName, metav1.GetOptions{}); err == nil {
		t.Fatal("unparsable cert reached the API")
	}
}

func TestMirrorOwnsIsLabelDriven(t *testing.T) {
	// The label is the ownership contract; a Secret this loop created always
	// carries it, a hand-created one (cert-manager, purchased cert) does not.
	if !mirrorOwns(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{managedByLabel: tlsMirrorManagedBy},
	}}) {
		t.Fatal("labeled Secret should be owned")
	}
	if mirrorOwns(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{managedByLabel: "cert-manager"},
	}}) {
		t.Fatal("foreign-labeled Secret must not be adopted")
	}
	if mirrorOwns(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: nil}}) {
		t.Fatal("unlabeled Secret must not be adopted")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
