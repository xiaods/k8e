package sandboxmcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/client"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// KIP-30 M2: identity self-bootstrap. The adapter enrolls its own gRPC client
// certificate from an API key instead of expecting an operator to generate and
// mount a client pair (the pre-M2 `mcp-gateway-tls` dance). The private key is
// generated in the pod; the issued certificate + the sandbox CA are cached in
// a ConfigMap so restarts and rollouts keep one identity until it nears its
// 90-day TTL, then re-enroll.
//
// Trust model (unchanged from the CLI):
//   - the API key is only ever sent over a channel whose server certificate
//     verifies against an explicit CA, the cached CA, or an explicit
//     trust-on-first-use opt-in;
//   - the server never sees a private key; the cached ConfigMap is written by
//     the pod itself, and the bootstrap API key in the pod's environment is
//     strictly more powerful than anything the cache can yield — an actor who
//     can read the cache can already read the API key and enroll a fresh
//     identity, so persisting the key next to the cert adds no exposure.

const (
	// defaultCertCacheName is the ConfigMap holding the enrolled identity.
	defaultCertCacheName = "mcp-client-cert"
	certCacheKeyCA       = "ca.crt"
	certCacheKeyCert     = "tls.crt"
	certCacheKeyKey      = "tls.key"
	// certRenewalDays mirrors pkg/sandbox/client: re-enroll when fewer days
	// remain, so a restart inside the final month never serves an expiring cert.
	certRenewalDays = 30
	// bootstrapTimeout bounds the enrollment Login at process start; the
	// Deployment's startupProbe tolerates a slow gateway for longer.
	bootstrapTimeout = 45 * time.Second
)

// EnrollmentConfig supplies everything a cold start needs to obtain a gateway
// client certificate.
type EnrollmentConfig struct {
	// Endpoint is the sandbox gRPC gateway address (host:port).
	Endpoint string
	// APIKey authorizes one Login RPC; empty means only cached material is
	// usable and an expired cache cannot be renewed.
	APIKey string
	// CAFile optionally pins the sandbox CA used to verify the gateway during
	// the Login handshake. When empty, the cached CA is used; when neither
	// exists, Insecure is required.
	CAFile string
	// Insecure opts into trust-on-first-use: send the API key to an
	// unverified server and pin the CA from the response. Same semantics as
	// the CLI's --insecure-bootstrap.
	Insecure bool
	// Device labels the identity in the server's issued-certificate ledger.
	Device string
}

// ClientIdentity is the PEM material of one enrolled gateway client.
type ClientIdentity struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

func (identity ClientIdentity) complete() bool {
	return len(identity.CertPEM) > 0 && len(identity.KeyPEM) > 0 && len(identity.CAPEM) > 0
}

// IdentityCache persists the enrolled identity across restarts so a rolling
// restart or rollout does not consume a fresh Login — and so every replica of
// one Deployment shares one auditable identity instead of N.
type IdentityCache interface {
	// Load returns nil, nil when no usable identity is cached.
	Load(ctx context.Context) (*ClientIdentity, error)
	// Store persists identity and returns the identity that should be used:
	// the stored one, or a concurrently enrolled one when another replica
	// won the create.
	Store(ctx context.Context, identity ClientIdentity) (*ClientIdentity, error)
}

// ConfigMapIdentityCache keeps the identity in one ConfigMap of the state
// namespace, writable through the mcp-state Role.
type ConfigMapIdentityCache struct {
	maps typedcore.ConfigMapInterface
	name string
}

// NewConfigMapIdentityCache binds the cache to a namespace-scoped ConfigMap
// client. An empty name disables caching; NewKubernetesStore-style nil checks
// keep the constructor honest.
func NewConfigMapIdentityCache(maps typedcore.ConfigMapInterface, name string) (*ConfigMapIdentityCache, error) {
	if maps == nil {
		return nil, errors.New("ConfigMap client required")
	}
	if name == "" {
		return nil, errors.New("identity cache ConfigMap name required")
	}
	return &ConfigMapIdentityCache{maps: maps, name: name}, nil
}

// Load reads the cached identity. Missing, partial or corrupt material is
// reported as "no cache" so the caller enrolls instead of failing forever.
func (cache *ConfigMapIdentityCache) Load(ctx context.Context) (*ClientIdentity, error) {
	configMap, err := cache.maps.Get(ctx, cache.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return identityFromConfigMap(configMap), nil
}

// Store writes the identity with first-writer-wins semantics: a concurrently
// created object that is still valid is adopted unchanged, while a stale or
// corrupt one is replaced in place. Either way the returned identity is the
// one the caller should dial with.
func (cache *ConfigMapIdentityCache) Store(ctx context.Context, identity ClientIdentity) (*ClientIdentity, error) {
	created, err := cache.maps.Create(ctx, cache.configMap(identity), metav1.CreateOptions{})
	if err == nil {
		return identityFromConfigMap(created), nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	existing, err := cache.maps.Get(ctx, cache.name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if other := identityFromConfigMap(existing); other != nil && identityFresh(other) {
		return other, nil
	}
	replacement := cache.configMap(identity)
	replacement.ResourceVersion = existing.ResourceVersion
	updated, err := cache.maps.Update(ctx, replacement, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return identityFromConfigMap(updated), nil
}

func (cache *ConfigMapIdentityCache) configMap(identity ClientIdentity) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cache.name,
			Labels: map[string]string{"k8e.sh/mcp-identity": "true"},
		},
		Data: map[string]string{
			certCacheKeyCA:   string(identity.CAPEM),
			certCacheKeyCert: string(identity.CertPEM),
			certCacheKeyKey:  string(identity.KeyPEM),
		},
	}
}

func identityFromConfigMap(configMap *corev1.ConfigMap) *ClientIdentity {
	if configMap == nil {
		return nil
	}
	identity := ClientIdentity{
		CertPEM: []byte(configMap.Data[certCacheKeyCert]),
		KeyPEM:  []byte(configMap.Data[certCacheKeyKey]),
		CAPEM:   []byte(configMap.Data[certCacheKeyCA]),
	}
	if !identity.complete() {
		return nil
	}
	return &identity
}

// identityFresh reports whether the cached certificate is usable right now,
// trusting the cached CA as the verification anchor (the same CA the gateway
// presented when it was issued).
func identityFresh(identity *ClientIdentity) bool {
	leaf, err := parseCertificatePEM(identity.CertPEM)
	if err != nil {
		return false
	}
	return certFresh(leaf, time.Now())
}

// certFresh splits validity from the renewal window so a still-valid but
// expiring certificate can be renewed proactively at startup.
func certFresh(leaf *x509.Certificate, now time.Time) bool {
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return false
	}
	return now.Before(leaf.NotAfter.Add(-time.Duration(certRenewalDays) * 24 * time.Hour))
}

func parseCertificatePEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// parseCachedKey reuses a cached private key when the certificate must be
// re-enrolled, so renewal refreshes the certificate without rotating the
// identity: the server ledger keeps one entry per Deployment, not per restart.
func parseCachedKey(keyPEM []byte) *ecdsa.PrivateKey {
	if len(keyPEM) == 0 {
		return nil
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil
	}
	return key
}

// ensureGatewayIdentity returns a dialable mTLS pair plus its CA pool. It
// reuses a fresh cached identity, renews an expiring one with the cached key,
// and enrolls from scratch when nothing usable is cached. Enrollment is the
// only step that needs the API key; a fresh cache makes the API key optional,
// which is what makes rotation-by-restart safe.
func ensureGatewayIdentity(ctx context.Context, cache IdentityCache, cfg EnrollmentConfig) (tls.Certificate, *x509.CertPool, error) {
	cached, err := loadIdentity(ctx, cache)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if cached != nil && identityFresh(cached) {
		return buildTLSMaterial(*cached)
	}
	if cfg.APIKey == "" {
		return tls.Certificate{}, nil, errors.New("gateway client certificate missing or expiring and no bootstrap API key to (re)enroll with; pass --bootstrap-api-key / K8E_MCP_BOOTSTRAP_API_KEY or stage explicit --gateway-cert/--gateway-key")
	}
	key, keyPEM, err := enrollmentKey(cached)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	csr, err := client.CreateCSR(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create CSR: %w", err)
	}
	anchor, err := trustAnchor(cfg, cached)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if anchor == nil && !cfg.Insecure {
		return tls.Certificate{}, nil, fmt.Errorf("no trust anchor for %s: pass --gateway-ca (the sandbox CA) or set --insecure-bootstrap for trust-on-first-use", cfg.Endpoint)
	}
	resp, err := client.Login(ctx, cfg.Endpoint, csr, client.LoginOptions{
		APIKey:     cfg.APIKey,
		CAPEM:      anchor,
		Insecure:   cfg.Insecure,
		DeviceName: cfg.Device,
	})
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("enroll with API key: %w", err)
	}
	if err := client.ValidateIssuedCertificate(resp, keyPEM); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("enrolled certificate rejected: %w", err)
	}
	enrolled := ClientIdentity{CertPEM: []byte(resp.Cert), KeyPEM: keyPEM, CAPEM: []byte(resp.CaCert)}
	published := enrolled
	if cache != nil {
		if adopted, err := cache.Store(ctx, enrolled); err == nil && adopted != nil && identityFresh(adopted) {
			published = *adopted
		}
		// A failed Store is not fatal: the in-memory identity still serves
		// this process; the next restart retries the cache.
	}
	return buildTLSMaterial(published)
}

// loadIdentity tolerates a nil cache (caching disabled) the same way the
// store does.
func loadIdentity(ctx context.Context, cache IdentityCache) (*ClientIdentity, error) {
	if cache == nil {
		return nil, nil
	}
	return cache.Load(ctx)
}

// enrollmentKey prefers the cached private key (identity continuity) and
// generates a fresh one only on a cold start or an unreadable cache.
func enrollmentKey(cached *ClientIdentity) (*ecdsa.PrivateKey, []byte, error) {
	if cached != nil {
		if key := parseCachedKey(cached.KeyPEM); key != nil {
			return key, cached.KeyPEM, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// trustAnchor picks the CA the Login handshake verifies the gateway with: an
// explicit file first (the operator's authoritative copy), then the cached CA
// (what the gateway presented at enrollment), then nothing — which requires
// the explicit trust-on-first-use opt-in before any API key leaves the pod.
func trustAnchor(cfg EnrollmentConfig, cached *ClientIdentity) ([]byte, error) {
	if cfg.CAFile != "" {
		pemBytes, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read gateway CA %s: %w", cfg.CAFile, err)
		}
		return pemBytes, nil
	}
	if cached != nil {
		return cached.CAPEM, nil
	}
	return nil, nil
}

func buildTLSMaterial(identity ClientIdentity) (tls.Certificate, *x509.CertPool, error) {
	pair, err := tls.X509KeyPair(identity.CertPEM, identity.KeyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load client certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(identity.CAPEM) {
		return tls.Certificate{}, nil, errors.New("cache CA has no certificates")
	}
	return pair, pool, nil
}
