// Package sandboxmcp implements the K8E Sandbox MCP server.
package sandboxmcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
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
		return nil, fmt.Errorf("sandbox mcp: tls: %w", err)
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("sandbox mcp: dial %s: %w", endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

var sessionGVR = k8sschema.GroupVersionResource{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxsessions"}

// FindActiveSession returns the session ID of an existing Active session for the given tenantID,
// or "" if none found. Used for cross-process session reuse.
func FindActiveSession(tenantID string) (string, error) {
	if tenantID == "" {
		return "", nil
	}
	var kubeconfigPath string
	for _, kc := range resolvedKubeconfigCandidates() {
		if _, err := os.Stat(kc); err == nil {
			kubeconfigPath = kc
			break
		}
	}
	if kubeconfigPath == "" {
		return "", nil
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return "", nil
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return "", nil
	}
	list, err := dyn.Resource(sessionGVR).Namespace("sandbox-matrix").List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		return "", nil
	}
	for i := range list.Items {
		data, _ := json.Marshal(list.Items[i].Object)
		var s sandboxv1.SandboxSession
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if s.Spec.TenantID == tenantID && s.Status.Phase == sandboxv1.SandboxPhaseActive {
			return s.Name, nil
		}
	}
	return "", nil
}

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
