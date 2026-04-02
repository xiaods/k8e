// Package sandboxmcp implements the K8E Sandbox MCP server.
package sandboxmcp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const defaultEndpoint = "127.0.0.1:50051"

var tlsCandidates = []string{
	"/var/lib/k8e/server/tls/serving-kube-apiserver.crt",
	"/etc/k8e/tls/serving-kube-apiserver.crt",
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
		return nil, fmt.Errorf("sandbox mcp: tls: %w", err)
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("sandbox mcp: dial %s: %w", endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func resolveCreds() (credentials.TransportCredentials, error) {
	// explicit env override
	if cert := os.Getenv("K8E_SANDBOX_CERT"); cert != "" {
		return credentials.NewClientTLSFromFile(cert, "")
	}
	// probe well-known paths
	for _, path := range tlsCandidates {
		if _, err := os.Stat(path); err == nil {
			return credentials.NewClientTLSFromFile(path, "")
		}
	}
	// fallback: system CA pool (remote cluster / insecure-skip not set)
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
}
