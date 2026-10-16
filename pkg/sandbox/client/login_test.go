package client

import (
	"bytes"
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
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const loginTestAPIKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// loginTestGateway serves Login over mTLS-free TLS with a throwaway CA so the
// exported Login path can be exercised against a real handshake.
type loginTestGateway struct {
	pb.SandboxServiceServer
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	logins int
}

func (gateway *loginTestGateway) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get("authorization")) == 0 || md.Get("authorization")[0] != "Bearer "+loginTestAPIKey {
		return nil, status.Error(codes.Unauthenticated, "invalid API key")
	}
	gateway.logins++
	block, _ := pem.Decode([]byte(req.Csr))
	if block == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "parse CSR")
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(int64(gateway.logins)),
		Subject:      pkix.Name{CommonName: "sandbox-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
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

// startLoginGateway returns a live loopback gateway, its CA PEM, and a cleanup.
func startLoginGateway(t *testing.T) (endpoint, caPEM string, gateway *loginTestGateway) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "login-test-ca"},
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
	gateway = &loginTestGateway{caKey: caKey, caCert: caCert}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverDER},
			PrivateKey:  serverKey,
		}},
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.NoClientCert,
	})))
	pb.RegisterSandboxServiceServer(server, gateway)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return listener.Addr().String(), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), gateway
}

func TestLoginIssuesAndValidatesCertificate(t *testing.T) {
	endpoint, caPEM, gateway := startLoginGateway(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := CreateCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := Login(ctx, endpoint, csr, LoginOptions{APIKey: loginTestAPIKey, CAPEM: []byte(caPEM), DeviceName: "login-test"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if gateway.logins != 1 {
		t.Fatalf("expected one Login RPC, got %d", gateway.logins)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := ValidateIssuedCertificate(resp, keyPEM); err != nil {
		t.Fatalf("issued certificate rejected: %v", err)
	}
	if !bytes.Equal([]byte(resp.CaCert), []byte(caPEM)) {
		t.Fatal("response CA differs from the trust anchor")
	}
}

func TestLoginRequiresTrustAnchorOrInsecureOptIn(t *testing.T) {
	endpoint, _, gateway := startLoginGateway(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := CreateCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Without a trust anchor and without the opt-in, the handshake must fail
	// rather than silently send the API key to an unverified server.
	if _, err := Login(ctx, endpoint, csr, LoginOptions{APIKey: loginTestAPIKey}); err == nil {
		t.Fatal("unverified server accepted without the insecure opt-in")
	}
	if gateway.logins != 0 {
		t.Fatalf("API key reached an unverified server: %d logins", gateway.logins)
	}
	// The explicit trust-on-first-use opt-in still works.
	if _, err := Login(ctx, endpoint, csr, LoginOptions{APIKey: loginTestAPIKey, Insecure: true}); err != nil {
		t.Fatalf("insecure bootstrap failed: %v", err)
	}
	if gateway.logins != 1 {
		t.Fatalf("expected one Login after opt-in, got %d", gateway.logins)
	}
}

func TestLoginRefusesWrongAPIKey(t *testing.T) {
	endpoint, caPEM, gateway := startLoginGateway(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := CreateCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = Login(ctx, endpoint, csr, LoginOptions{APIKey: "not-the-key", CAPEM: []byte(caPEM)})
	if err == nil || status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong API key must be rejected: %v", err)
	}
	if gateway.logins != 0 {
		t.Fatalf("Login reached the signer with a bad key: %d", gateway.logins)
	}
}

func TestLoginRefusesMissingAPIKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Login(ctx, "127.0.0.1:50051", "csr", LoginOptions{}); err == nil {
		t.Fatal("missing API key must fail before any dial")
	}
}
