// Package client provides the gRPC client for K8E sandbox operations.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// resolvedKubeconfigCandidates returns kubeconfig paths including KUBECONFIG env and ~/.kube/config.
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
		return nil, fmt.Errorf("sandbox client: dial %s: %w", endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// NewClientWithEndpoint connects to a remote K8E cluster at endpoint with API key.
// On first connection, auto-downloads the server CA cert via TOFU for future use.
func NewClientWithEndpoint(endpoint, apiKey string) (*Client, error) {
	if apiKey == "" {
		return NewClient()
	}
	cacheDir, _ := sandboxCacheDir()
	caFile := filepath.Join(cacheDir, "ca.crt")

	c, cacheErr := tryCachedCert(endpoint, apiKey, caFile)
	if c != nil {
		return c, nil
	}
	if cacheErr != nil && !isCertError(cacheErr) {
		return nil, fmt.Errorf("sandbox client: %w", cacheErr)
	}
	// nil error (no cache) or cert error (expired/rotated) — clear and TOFU
	os.Remove(caFile) //nolint:errcheck

	return tofuConnect(endpoint, apiKey, caFile)
}

// tryCachedCert attempts a verified connection using the cached CA cert.
// Validates the connection with a lightweight GetCACert RPC so TLS and
// auth errors surface immediately. Returns (nil, nil) when no cache exists.
func tryCachedCert(endpoint, apiKey, caFile string) (*Client, error) {
	if _, err := os.Stat(caFile); err != nil {
		return nil, nil
	}
	creds, err := credentials.NewClientTLSFromFile(caFile, "")
	if err != nil {
		return nil, err
	}
	conn, err := dialWithAPIKey(endpoint, apiKey, creds)
	if err != nil {
		return nil, err
	}
	// Trigger connection establishment — TLS/auth errors surface here.
	_, rpcErr := pb.NewSandboxServiceClient(conn).GetCACert(
		context.Background(), &pb.GetCACertRequest{},
	)
	if rpcErr != nil {
		conn.Close()
		return nil, rpcErr
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// tofuConnect performs Trust-On-First-Use: downloads the server CA cert over
// an API-key-authenticated insecure connection, verifies the TLS handshake
// fingerprint matches, caches the cert, and reconnects with full TLS verification.
func tofuConnect(endpoint, apiKey, caFile string) (*Client, error) {
	peerCerts, tofuCreds := newTOFUCredentials()

	certPEM, err := downloadCACert(endpoint, apiKey, tofuCreds)
	if err != nil {
		return nil, err
	}
	if err := verifyFingerprint(*peerCerts, certPEM); err != nil {
		return nil, err
	}
	if err := cacheCert(caFile, certPEM); err != nil {
		return nil, err
	}
	return connectVerified(endpoint, apiKey, caFile)
}

// newTOFUCredentials creates TLS credentials that skip server certificate
// validation — intentionally. The insecure connection exists only for a single
// GetCACert RPC authenticated with an API key, and the server's certificate
// fingerprint is verified against the GetCACert response before any other
// data is exchanged. After verification, the connection is discarded and a
// fully-verified connection is established.
func newTOFUCredentials() (*[]*x509.Certificate, credentials.TransportCredentials) {
	peerCerts := &[]*x509.Certificate{}
	//nolint:gosec // intentional TOFU, MITM blocked by verifyFingerprint
	creds := credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // NOSONAR — TOFU bootstrap, verified immediately after
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				if cert, err := x509.ParseCertificate(raw); err == nil {
					*peerCerts = append(*peerCerts, cert)
				}
			}
			return nil
		},
	})
	return peerCerts, creds
}

// downloadCACert opens an insecure connection, calls GetCACert, and immediately
// closes the connection. The returned cert PEM is not yet trusted — callers
// must verify its fingerprint against the TLS handshake peer certificate.
func downloadCACert(endpoint, apiKey string, creds credentials.TransportCredentials) (string, error) {
	conn, err := dialWithAPIKey(endpoint, apiKey, creds)
	if err != nil {
		return "", fmt.Errorf("sandbox client: dial %s: %w", endpoint, err)
	}
	defer conn.Close()

	resp, err := pb.NewSandboxServiceClient(conn).GetCACert(
		context.Background(), &pb.GetCACertRequest{},
	)
	if err != nil {
		return "", fmt.Errorf("sandbox client: get CA cert: %w", err)
	}
	if resp.Cert == "" {
		return "", fmt.Errorf("sandbox client: server returned empty CA cert")
	}
	return resp.Cert, nil
}

// verifyFingerprint checks that the certificate presented during the TLS
// handshake matches the one returned by GetCACert. A mismatch indicates a
// MITM attack where the attacker presented a different cert during handshake
// than they returned through the authenticated RPC.
func verifyFingerprint(peerCerts []*x509.Certificate, certPEM string) error {
	if len(peerCerts) == 0 {
		return fmt.Errorf("sandbox client: no peer certificate captured during TLS handshake")
	}
	pemBlock, _ := pem.Decode([]byte(certPEM))
	if pemBlock == nil {
		return fmt.Errorf("sandbox client: invalid PEM in server CA cert")
	}
	downloadedCert, err := x509.ParseCertificate(pemBlock.Bytes)
	if err != nil {
		return fmt.Errorf("sandbox client: parse server CA cert: %w", err)
	}
	if !bytes.Equal(sha256sum(peerCerts[0].Raw), sha256sum(downloadedCert.Raw)) {
		return fmt.Errorf("sandbox client: TLS certificate fingerprint mismatch — possible MITM attack")
	}
	return nil
}

// sha256sum returns the SHA-256 hash of data as a byte slice.
func sha256sum(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// cacheCert writes the downloaded CA certificate to the local cache directory.
func cacheCert(caFile, certPEM string) error {
	if err := os.MkdirAll(filepath.Dir(caFile), 0700); err != nil {
		return fmt.Errorf("sandbox client: create cache dir: %w", err)
	}
	if err := os.WriteFile(caFile, []byte(certPEM), 0644); err != nil {
		return fmt.Errorf("sandbox client: save cert: %w", err)
	}
	return nil
}

// connectVerified establishes a gRPC connection with full TLS verification
// using the CA certificate at caFile and the provided API key.
func connectVerified(endpoint, apiKey, caFile string) (*Client, error) {
	creds, err := credentials.NewClientTLSFromFile(caFile, "")
	if err != nil {
		return nil, fmt.Errorf("sandbox client: load downloaded cert: %w", err)
	}
	conn, err := dialWithAPIKey(endpoint, apiKey, creds)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: verified dial %s: %w", endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// dialWithAPIKey creates a gRPC connection with TLS creds and API key auth interceptor.
func dialWithAPIKey(endpoint, apiKey string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	injectAuth := func(ctx context.Context) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+apiKey)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	opts = append(opts,
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(injectAuth(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(injectAuth(ctx), desc, cc, method, opts...)
		}),
	)
	return grpc.NewClient(endpoint, opts...)
}

// isCertError reports whether err is caused by a TLS certificate validation
// failure (unknown authority, hostname mismatch, expired cert, etc.) as
// opposed to a network error (connection refused, timeout, DNS failure).
func isCertError(err error) bool {
	var unknownAuth x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var constraint x509.ConstraintViolationError
	if errors.As(err, &unknownAuth) || errors.As(err, &certInvalid) ||
		errors.As(err, &hostname) || errors.As(err, &constraint) {
		return true
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		msg := e.Error()
		if len(msg) > 5 && (msg[:5] == "tls: " || msg[:5] == "x509:") {
			return true
		}
	}
	return false
}

func sandboxCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".k8e", "sandbox"), nil
}

func (c *Client) Close() error { return c.conn.Close() }

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
		if decoded, err := base64.StdEncoding.DecodeString(string(caData)); err == nil {
			caData = decoded
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caData) {
			return credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
		}
	}
	return nil, fmt.Errorf("no valid CA found in %s", path)
}
