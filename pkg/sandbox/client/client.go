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

// NewClientWithEndpoint connects to a remote K8E cluster at endpoint with optional API key.
// On first connection, auto-downloads the server CA cert for future verified connections.
func NewClientWithEndpoint(endpoint, apiKey string) (*Client, error) {
	if apiKey == "" {
		return NewClient()
	}

	// Try cached CA cert first with a blocking dial so TLS errors surface immediately.
	// Only certificate validation errors trigger TOFU; network errors are returned as-is
	// to avoid deleting a valid cache during transient outages.
	cacheDir, _ := sandboxCacheDir()
	caFile := filepath.Join(cacheDir, "ca.crt")
	if _, err := os.Stat(caFile); err == nil {
		creds, err := credentials.NewClientTLSFromFile(caFile, "")
		if err == nil {
			conn, err := dialWithAPIKey(endpoint, apiKey, creds, true)
			if err == nil {
				return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
			}
			if !isCertError(err) {
				return nil, fmt.Errorf("sandbox client: %w", err)
			}
		}
		// Cached cert is invalid (malformed or server cert has changed) —
		// clear and fall through to TOFU to refresh the pinned certificate.
		os.Remove(caFile) //nolint:errcheck
	}

	// Trust-On-First-Use (TOFU): connect without cert verification to download
	// the server CA cert via GetCACert (authenticated with API key). The TLS
	// handshake peer certificate fingerprint is captured and compared against
	// the downloaded cert to detect MITM. After verification, the insecure
	// connection is closed and a new verified connection is established.
	var peerCerts []*x509.Certificate
	tofuCreds := credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				if cert, err := x509.ParseCertificate(raw); err == nil {
					peerCerts = append(peerCerts, cert)
				}
			}
			return nil
		},
	})
	//nolint:gosec // intentional TOFU pattern, MITM detected via fingerprint comparison
	tofuConn, err := dialWithAPIKey(endpoint, apiKey, tofuCreds, false)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: dial %s: %w", endpoint, err)
	}
	tofuClient := pb.NewSandboxServiceClient(tofuConn)

	resp, err := tofuClient.GetCACert(context.Background(), &pb.GetCACertRequest{})
	tofuConn.Close() // immediately close insecure connection
	if err != nil {
		return nil, fmt.Errorf("sandbox client: get CA cert: %w", err)
	}
	if resp.Cert == "" {
		return nil, fmt.Errorf("sandbox client: server returned empty CA cert")
	}

	// Verify the downloaded cert matches what was presented during TLS handshake
	if len(peerCerts) == 0 {
		return nil, fmt.Errorf("sandbox client: no peer certificate captured during TLS handshake")
	}
	downloadedPEM, _ := pem.Decode([]byte(resp.Cert))
	if downloadedPEM == nil {
		return nil, fmt.Errorf("sandbox client: invalid PEM in server CA cert")
	}
	downloadedCert, err := x509.ParseCertificate(downloadedPEM.Bytes)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: parse server CA cert: %w", err)
	}
	peerFP := sha256.Sum256(peerCerts[0].Raw)
	dlFP := sha256.Sum256(downloadedCert.Raw)
	if !bytes.Equal(peerFP[:], dlFP[:]) {
		return nil, fmt.Errorf("sandbox client: TLS certificate fingerprint mismatch — possible MITM attack")
	}

	// Save cert to cache for future connections
	os.MkdirAll(cacheDir, 0700) //nolint:errcheck
	os.WriteFile(caFile, []byte(resp.Cert), 0644) //nolint:errcheck

	// Reconnect with full TLS verification using the downloaded cert
	verifiedCreds, err := credentials.NewClientTLSFromFile(caFile, "")
	if err != nil {
		return nil, fmt.Errorf("sandbox client: load downloaded cert: %w", err)
	}
	conn, err := dialWithAPIKey(endpoint, apiKey, verifiedCreds, false)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: verified dial %s: %w", endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// dialWithAPIKey connects with TLS creds and API key auth interceptor.
// When block is true, the dial blocks until the connection is established
// or the 10s timeout expires — errors (including TLS verification failures)
// are returned immediately instead of surfacing on the first RPC.
func dialWithAPIKey(endpoint, apiKey string, creds credentials.TransportCredentials, block bool) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	opts = append(opts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+apiKey)
		return invoker(ctx, method, req, reply, cc, opts...)
	}))
	if block {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		opts = append(opts, grpc.WithBlock(), grpc.WithReturnConnectionError())
		return grpc.DialContext(ctx, endpoint, opts...)
	}
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
	// gRPC may nest the TLS error deeper; check wrapped errors too
	for err != nil {
		errStr := err.Error()
		if len(errStr) > 5 && (errStr[:5] == "tls: " || errStr[:5] == "x509:") {
			return true
		}
		err = errors.Unwrap(err)
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
	// explicit env override — support both CA-only and mTLS (cert+key)
	if cert := os.Getenv("K8E_SANDBOX_CERT"); cert != "" {
		if key := os.Getenv("K8E_SANDBOX_KEY"); key != "" {
			// mTLS: client cert + key
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
	// probe well-known paths
	for _, path := range tlsCandidates {
		if _, err := os.Stat(path); err == nil {
			return credentials.NewClientTLSFromFile(path, "")
		}
	}
	// probe kubeconfig CA
	for _, kc := range resolvedKubeconfigCandidates() {
		if creds, err := credsFromKubeconfig(kc); err == nil {
			return creds, nil
		}
	}
	// fallback: system CA pool (remote cluster / insecure-skip not set)
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
		// caData may be base64-encoded in some kubeconfig formats
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
