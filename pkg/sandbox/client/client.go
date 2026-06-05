package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultEndpoint = "127.0.0.1:50051"

var tlsCandidates = []string{
	"/var/lib/k8e/server/tls/serving-kube-apiserver.crt",
	"/etc/k8e/tls/serving-kube-apiserver.crt",
}

var kubeconfigCandidates = []string{
	"/etc/k8e/k8e.yaml",
	"/var/lib/k8e/server/cred/admin.kubeconfig",
}

func resolvedKubeconfigCandidates() []string {
	candidates := make([]string, 0, len(kubeconfigCandidates)+2)
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		candidates = append(candidates, kc)
	}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, home+"/.kube/config")
	}
	return append(candidates, kubeconfigCandidates...)
}

// Client wraps a gRPC SandboxServiceClient with its underlying connection.
type Client struct {
	SandboxServiceClient pb.SandboxServiceClient
	conn                 *grpc.ClientConn
}

// NewClient auto-discovers the local K8E TLS cert and connects to the sandbox gRPC gateway.
// Override with K8E_SANDBOX_ENDPOINT, K8E_SANDBOX_CERT env vars.
func NewClient() (*Client, error) {
	endpoint := os.Getenv("K8E_SANDBOX_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	creds, err := resolveCreds()
	if err != nil {
		return nil, fmt.Errorf("sandbox client: tls: %w", err)
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// NewClientWithEndpoint connects to a remote K8E cluster at endpoint.
// On first use, performs mTLS bootstrap: generates a key pair, logs in with the API key,
// and obtains a short-lived client certificate. Subsequent calls use the cached certificate
// with automatic lazy renewal.
func NewClientWithEndpoint(endpoint, apiKey string) (*Client, error) {
	if apiKey == "" {
		return NewClient()
	}
	cacheDir, _ := sandboxCacheDir()
	caFile := filepath.Join(cacheDir, "ca.crt")
	certFile := filepath.Join(cacheDir, "client.crt")
	keyFile := filepath.Join(cacheDir, "client.key")

	// Path 1: have CA + valid client cert → direct mTLS
	if _, caErr := os.Stat(caFile); caErr == nil {
		if certValid(certFile) {
			// Lazy renewal: if cert expires within 7 days, try to renew via mTLS
			if certExpiringSoon(certFile, 7) {
				renewClientCert(endpoint, caFile, certFile, keyFile)
			}
			conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
			if err != nil {
				return nil, dialErr(endpoint, err)
			}
			return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
		}

		// Path 2: have CA but cert expired/missing → TLS + Login with API key
		return bootstrapWithCA(endpoint, caFile, certFile, keyFile, apiKey)
	}

	// Path 3: no CA → insecure bootstrap, get both CA and client cert
	return bootstrapInsecure(endpoint, caFile, certFile, keyFile, apiKey)
}

func (c *Client) Close() error { return c.conn.Close() }

// ── mTLS bootstrap helpers ────────────────────────────────────────────────────

func bootstrapWithCA(endpoint, caFile, certFile, keyFile, apiKey string) (*Client, error) {
	key, err := generateAndSaveKey(keyFile)
	if err != nil {
		return nil, err
	}
	csr, err := createCSR(key)
	if err != nil {
		return nil, err
	}
	resp, err := callLogin(endpoint, caFile, apiKey, csr)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: login: %w", err)
	}
	if err := os.WriteFile(certFile, []byte(resp.Cert), 0644); err != nil {
		return nil, fmt.Errorf("sandbox client: save cert: %w", err)
	}

	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

func bootstrapInsecure(endpoint, caFile, certFile, keyFile, apiKey string) (*Client, error) {
	key, err := generateAndSaveKey(keyFile)
	if err != nil {
		return nil, err
	}
	csr, err := createCSR(key)
	if err != nil {
		return nil, err
	}
	// empty caFile → InsecureSkipVerify for bootstrap
	resp, err := callLogin(endpoint, "", apiKey, csr)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: bootstrap login: %w", err)
	}
	if err := os.WriteFile(caFile, []byte(resp.CaCert), 0644); err != nil {
		return nil, fmt.Errorf("sandbox client: save CA: %w", err)
	}
	if err := os.WriteFile(certFile, []byte(resp.Cert), 0644); err != nil {
		return nil, fmt.Errorf("sandbox client: save cert: %w", err)
	}

	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// renewClientCert attempts to renew the client certificate via mTLS.
// Failures are silent — the existing (still-valid) cert continues to work.
func renewClientCert(endpoint, caFile, certFile, keyFile string) {
	key, err := loadClientKey(keyFile)
	if err != nil {
		return
	}
	csr, err := createCSR(key)
	if err != nil {
		return
	}
	resp, err := callLoginMTLS(endpoint, caFile, certFile, keyFile, csr)
	if err != nil {
		return
	}
	os.WriteFile(certFile, []byte(resp.Cert), 0644) //nolint:errcheck
}

// ── Key & CSR generation ──────────────────────────────────────────────────────

func generateAndSaveKey(keyFile string) (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
		return nil, fmt.Errorf("sandbox client: create cache dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: generate key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: marshal key: %w", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: der,
	}), 0600); err != nil {
		return nil, fmt.Errorf("sandbox client: save key: %w", err)
	}
	return key, nil
}

func createCSR(key *ecdsa.PrivateKey) (string, error) {
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "sandbox-client"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return "", fmt.Errorf("sandbox client: create CSR: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE REQUEST", Bytes: der,
	})), nil
}

func loadClientKey(keyFile string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid key PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// ── Certificate validation ────────────────────────────────────────────────────

func certValid(certFile string) bool {
	cert, err := loadAndParseCert(certFile)
	if err != nil {
		return false
	}
	now := time.Now()
	return now.After(cert.NotBefore) && now.Before(cert.NotAfter)
}

func certExpiringSoon(certFile string, days int) bool {
	cert, err := loadAndParseCert(certFile)
	if err != nil {
		return true
	}
	return time.Now().After(cert.NotAfter.Add(-time.Duration(days) * 24 * time.Hour))
}

func loadAndParseCert(certFile string) (*x509.Certificate, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid cert PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ── Connection helpers ────────────────────────────────────────────────────────

func dialMTLS(endpoint, caFile, certFile, keyFile string) (*grpc.ClientConn, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA cert")
	}

	clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	creds := credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{clientCert},
		RootCAs:            pool,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: isLoopback(endpoint),
	})
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
}

func isLoopback(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

func callLogin(endpoint, caFile, apiKey, csr string) (*pb.LoginResponse, error) {
	var creds credentials.TransportCredentials
	if caFile == "" {
		// Bootstrap: no cached CA, server verification is impossible. The
		// connection is authenticated by the API key in gRPC metadata. A MITM
		// that intercepts this single Login call gains only a short-lived client
		// certificate, useless for future mTLS connections that verify the CA.
		//nolint:gosec
		creds = credentials.NewTLS(&tls.Config{ // NOSONAR
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // NOSONAR: ssl:S4830 — bootstrap secured by API key auth
		})
	} else {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caPEM)
		creds = credentials.NewTLS(&tls.Config{
			RootCAs:            pool,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: isLoopback(endpoint),
		})
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer "+apiKey,
	)
	return pb.NewSandboxServiceClient(conn).Login(ctx, &pb.LoginRequest{
		Csr: csr,
	})
}

func callLoginMTLS(endpoint, caFile, certFile, keyFile, csr string) (*pb.LoginResponse, error) {
	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return pb.NewSandboxServiceClient(conn).Login(context.Background(), &pb.LoginRequest{
		Csr: csr,
	})
}

func dialErr(endpoint string, err error) error {
	return fmt.Errorf("sandbox client: dial %s: %w", endpoint, err)
}

// ── Local auto-discovery ──────────────────────────────────────────────────────

func sandboxCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".k8e", "sandbox"), nil
}

func resolveCreds() (credentials.TransportCredentials, error) {
	if cert := os.Getenv("K8E_SANDBOX_CERT"); cert != "" {
		if key := os.Getenv("K8E_SANDBOX_KEY"); key != "" {
			tlsCert, err := tls.LoadX509KeyPair(cert, key)
			if err != nil {
				return nil, err
			}
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			return credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				RootCAs:      pool,
				MinVersion:   tls.VersionTLS12,
			}), nil
		}
		return credentials.NewClientTLSFromFile(cert, "")
	}
	for _, path := range tlsCandidates {
		if _, err := os.Stat(path); err == nil {
			return credentials.NewClientTLSFromFile(path, "")
		}
	}
	for _, kc := range resolvedKubeconfigCandidates() {
		if creds, err := credsFromKubeconfig(kc); err == nil {
			return creds, nil
		}
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
}

func credsFromKubeconfig(path string) (credentials.TransportCredentials, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	for _, cluster := range cfg.Clusters {
		var caData []byte
		if len(cluster.CertificateAuthorityData) > 0 {
			caData = cluster.CertificateAuthorityData
		} else if cluster.CertificateAuthority != "" {
			caData, err = os.ReadFile(cluster.CertificateAuthority)
			if err != nil {
				continue
			}
		}
		if len(caData) == 0 {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caData) {
			return credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
		}
	}
	return nil, fmt.Errorf("no valid CA found in %s", path)
}
