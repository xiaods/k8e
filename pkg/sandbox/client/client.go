// Package client provides the gRPC client for K8E sandbox operations.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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

// NewClientWithEndpoint connects to a remote K8E cluster at endpoint with optional API key.
// On first connection, auto-downloads the server CA cert for future verified connections.
func NewClientWithEndpoint(endpoint, apiKey string) (*Client, error) {
	if apiKey == "" {
		return NewClient()
	}

	// Try cached CA cert first
	cacheDir, _ := sandboxCacheDir()
	caFile := filepath.Join(cacheDir, "ca.crt")
	if _, err := os.Stat(caFile); err == nil {
		creds, err := credentials.NewClientTLSFromFile(caFile, "")
		if err == nil {
			conn, err := dialWithAPIKey(endpoint, apiKey, creds)
			if err == nil {
				return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
			}
		}
	}

	// Trust-On-First-Use (TOFU): connect without cert verification to download
	// the server CA cert via GetCACert (authenticated with API key). Once the cert
	// is saved locally, reconnect with full TLS verification for all subsequent calls.
	// The InsecureSkipVerify window is a single GetCACert RPC protected by API key auth.
	//nolint:gosec // intentional TOFU pattern
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
	conn, err := dialWithAPIKey(endpoint, apiKey, creds)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: dial %s: %w", endpoint, err)
	}
	client := &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}

	// Download CA cert for future use (cache only, don't disrupt current connection)
	resp, err := client.SandboxServiceClient.GetCACert(context.Background(), &pb.GetCACertRequest{})
	if err == nil && resp.Cert != "" {
		os.MkdirAll(cacheDir, 0700) //nolint:errcheck
		os.WriteFile(caFile, []byte(resp.Cert), 0644) //nolint:errcheck
	}
	return client, nil
}

// dialWithAPIKey connects with TLS creds and API key auth interceptor.
func dialWithAPIKey(endpoint, apiKey string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	opts = append(opts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+apiKey)
		return invoker(ctx, method, req, reply, cc, opts...)
	}))
	return grpc.NewClient(endpoint, opts...)
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
