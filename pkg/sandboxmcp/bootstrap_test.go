package sandboxmcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const bootstrapTestAPIKey = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// mcpLoginGateway serves Login over TLS with a throwaway sandbox CA so the
// bootstrap exercises a real handshake, not a stub.
type mcpLoginGateway struct {
	pb.SandboxServiceServer
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	ttl    time.Duration
	mu     sync.Mutex
	logins int
}

func (gateway *mcpLoginGateway) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get("authorization")) == 0 || md.Get("authorization")[0] != "Bearer "+bootstrapTestAPIKey {
		return nil, status.Error(codes.Unauthenticated, "invalid API key")
	}
	block, _ := pem.Decode([]byte(req.Csr))
	if block == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "parse CSR")
	}
	gateway.mu.Lock()
	gateway.logins++
	count := gateway.logins
	ttl := gateway.ttl
	gateway.mu.Unlock()
	if ttl == 0 {
		ttl = 90 * 24 * time.Hour
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(int64(count)),
		Subject:      pkix.Name{CommonName: "sandbox-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, gateway.caCert, csr.PublicKey, gateway.caKey)
	if err != nil {
		return nil, status.Error(codes.Internal, "sign")
	}
	return &pb.LoginResponse{
		Cert:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CaCert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.caCert.Raw})),
	}, nil
}

// startMCPLoginGateway returns a live loopback gateway, its CA PEM and a
// cleanup. ttl overrides the default 90-day client cert lifetime.
func startMCPLoginGateway(t *testing.T, ttl time.Duration) (endpoint, caFile string, gateway *mcpLoginGateway) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mcp-bootstrap-test-ca"},
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
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "sandbox-grpc-gateway"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"sandbox-grpc-gateway.sandbox-matrix.svc"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gateway = &mcpLoginGateway{caKey: caKey, caCert: caCert, ttl: ttl}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.NoClientCert,
	})))
	pb.RegisterSandboxServiceServer(server, gateway)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	caFile = filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644); err != nil {
		t.Fatal(err)
	}
	return listener.Addr().String(), caFile, gateway
}

func testIdentityCache(t *testing.T) IdentityCache {
	t.Helper()
	cache, err := NewConfigMapIdentityCache(fake.NewSimpleClientset().CoreV1().ConfigMaps("k8e-mcp"), defaultCertCacheName)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func bootstrapContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 20*time.Second)
}

// seedIdentity signs a client pair over key directly with the gateway CA so
// tests can control the cached certificate's expiry window.
func seedIdentity(t *testing.T, gateway *mcpLoginGateway, key *ecdsa.PrivateKey, notAfter time.Time) ClientIdentity {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "sandbox-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, gateway.caCert, &key.PublicKey, gateway.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return ClientIdentity{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CAPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.caCert.Raw}),
	}
}

func cachedConfigMapData(t *testing.T, cache IdentityCache) map[string]string {
	t.Helper()
	configMaps := cache.(*ConfigMapIdentityCache)
	configMap, err := configMaps.maps.Get(context.Background(), configMaps.name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("identity cache missing after bootstrap: %v", err)
	}
	return configMap.Data
}

func TestEnsureGatewayIdentityEnrollsCachesAndReuses(t *testing.T) {
	endpoint, caFile, gateway := startMCPLoginGateway(t, 0)
	cache := testIdentityCache(t)
	ctx, cancel := bootstrapContext(t)
	defer cancel()
	identity, pool, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint, APIKey: bootstrapTestAPIKey, CAFile: caFile, Device: "k8e-mcp-test"})
	if err != nil {
		t.Fatalf("cold start enrollment failed: %v", err)
	}
	if gateway.logins != 1 {
		t.Fatalf("expected one Login, got %d", gateway.logins)
	}
	if identity.Leaf == nil || identity.Leaf.Subject.CommonName != "sandbox-client" {
		t.Fatalf("no usable client certificate: %+v", identity.Leaf)
	}
	if pool == nil {
		t.Fatal("no CA pool built")
	}
	data := cachedConfigMapData(t, cache)
	if data["ca.crt"] == "" || data["tls.crt"] == "" || data["tls.key"] == "" {
		t.Fatalf("identity cache incomplete: %v", data)
	}
	// A warm cache serves the restart without another Login or an API key.
	restarted, _, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint})
	if err != nil {
		t.Fatalf("warm restart failed: %v", err)
	}
	if gateway.logins != 1 {
		t.Fatalf("cache miss caused a re-login: %d", gateway.logins)
	}
	if len(restarted.Certificate) != len(identity.Certificate) {
		t.Fatal("restart did not reuse the cached certificate")
	}
}

func TestEnsureGatewayIdentityRenewsExpiringCertWithSameKey(t *testing.T) {
	endpoint, caFile, gateway := startMCPLoginGateway(t, 0)
	cache := testIdentityCache(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Inside the 30-day renewal window but still valid: a restart refreshes
	// the certificate, keeping the private key (and so the identity) stable.
	if _, err := cache.Store(context.Background(), seedIdentity(t, gateway, key, time.Now().Add(10*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := bootstrapContext(t)
	defer cancel()
	identity, _, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint, APIKey: bootstrapTestAPIKey, CAFile: caFile})
	if err != nil {
		t.Fatalf("renewal failed: %v", err)
	}
	if gateway.logins != 1 {
		t.Fatalf("expected one re-enrollment Login, got %d", gateway.logins)
	}
	if identity.Leaf == nil || !identity.Leaf.NotAfter.After(time.Now().Add(30*24*time.Hour)) {
		t.Fatalf("renewed certificate not fresh: %v", identity.Leaf)
	}
	publicKey, ok := identity.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("expected an EC private key")
	}
	if publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
		t.Fatal("renewal rotated the identity's private key")
	}
}

func TestEnsureGatewayIdentityExpiredCacheNeedsAPIKey(t *testing.T) {
	endpoint, caFile, gateway := startMCPLoginGateway(t, 0)
	cache := testIdentityCache(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Store(context.Background(), seedIdentity(t, gateway, key, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := bootstrapContext(t)
	defer cancel()
	if _, _, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint}); err == nil {
		t.Fatal("expired cache without an API key must fail loudly")
	}
	if gateway.logins != 0 {
		t.Fatalf("relogin attempted without an API key: %d", gateway.logins)
	}
	identity, _, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint, APIKey: bootstrapTestAPIKey, CAFile: caFile})
	if err != nil {
		t.Fatalf("re-enrollment from expired cache failed: %v", err)
	}
	if identity.Leaf == nil {
		t.Fatal("no certificate after re-enrollment")
	}
}

func TestEnsureGatewayIdentityRefusesUnverifiedFirstContact(t *testing.T) {
	endpoint, _, gateway := startMCPLoginGateway(t, 0)
	cache := testIdentityCache(t)
	ctx, cancel := bootstrapContext(t)
	defer cancel()
	_, _, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint, APIKey: bootstrapTestAPIKey})
	if err == nil {
		t.Fatal("enrollment must not send the API key without a trust anchor")
	}
	if gateway.logins != 0 {
		t.Fatalf("API key reached an unverified gateway: %d", gateway.logins)
	}
	// Trust-on-first-use is the explicit escape hatch.
	if _, pool, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint, APIKey: bootstrapTestAPIKey, Insecure: true}); err != nil {
		t.Fatalf("insecure bootstrap failed: %v", err)
	} else if pool == nil {
		t.Fatal("insecure bootstrap pinned no CA from the response")
	}
	if gateway.logins != 1 {
		t.Fatalf("expected one opt-in Login, got %d", gateway.logins)
	}
}

func TestEnsureGatewayIdentityPrefersExplicitCAOverCache(t *testing.T) {
	endpoint, _, gateway := startMCPLoginGateway(t, 0)
	cache := testIdentityCache(t)
	// An unrelated cached CA must not win over the operator's file.
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherCA := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "unrelated-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	otherDER, err := x509.CreateCertificate(rand.Reader, otherCA, otherCA, &otherKey.PublicKey, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherDER})
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	expiredLeaf := &x509.Certificate{
		SerialNumber: big.NewInt(9),
		Subject:      pkix.Name{CommonName: "stale-sandbox-client"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	expiredDER, err := x509.CreateCertificate(rand.Reader, expiredLeaf, otherCA, &leafKey.PublicKey, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Store(context.Background(), ClientIdentity{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: expiredDER}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		CAPEM:   caPEM,
	}); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.caCert.Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := bootstrapContext(t)
	defer cancel()
	if _, _, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{Endpoint: endpoint, APIKey: bootstrapTestAPIKey, CAFile: caFile}); err != nil {
		// The cached placeholder cert is not fresh, so enrollment runs; the
		// explicit CA verifies the gateway and the response validates.
		t.Fatalf("explicit CA over cached CA failed: %v", err)
	}
	if gateway.logins != 1 {
		t.Fatalf("expected one enrollment Login, got %d", gateway.logins)
	}
}

func TestIdentityCacheStoreAdoptsValidWinner(t *testing.T) {
	_, _, gateway := startMCPLoginGateway(t, 0)
	cache := testIdentityCache(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	winner := seedIdentity(t, gateway, key, time.Now().Add(90*24*time.Hour))
	if _, err := cache.Store(context.Background(), winner); err != nil {
		t.Fatal(err)
	}
	loserKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	loser := seedIdentity(t, gateway, loserKey, time.Now().Add(90*24*time.Hour))
	adopted, err := cache.Store(context.Background(), loser)
	if err != nil {
		t.Fatalf("concurrent store: %v", err)
	}
	if string(adopted.CertPEM) != string(winner.CertPEM) {
		t.Fatal("store did not adopt the first writer's identity")
	}
}

func TestIdentityCacheStoreReplacesCorruptEntry(t *testing.T) {
	cache := testIdentityCache(t)
	configMaps := cache.(*ConfigMapIdentityCache)
	// Partial data cannot be an identity, so Load treats it as missing and an
	// enrollment that follows must overwrite instead of adopting garbage.
	if _, err := configMaps.maps.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMaps.name},
		Data:       map[string]string{"tls.crt": "half-baked"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh := ClientIdentity{CertPEM: []byte("cert"), KeyPEM: []byte("key"), CAPEM: []byte("ca")}
	if _, err := cache.Store(context.Background(), fresh); err != nil {
		t.Fatalf("replace corrupt entry: %v", err)
	}
	configMap, err := configMaps.maps.Get(context.Background(), configMaps.name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if configMap.Data[certCacheKeyKey] != "key" || configMap.Data[certCacheKeyCert] != "cert" {
		t.Fatalf("corrupt entry not replaced: %v", configMap.Data)
	}
}
