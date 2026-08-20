package grpc

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/xiaods/k8e/pkg/sandbox/client"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// freePort reserves an ephemeral port by binding :0 and closing; good enough
// for tests (Start re-checks liveness before binding).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway did not start listening on %d", port)
}

// TestTLSGatewayLoop verifies the full mTLS bootstrap + status path over a
// real gateway: server issues its sandbox CA + server cert, the client
// bootstraps via Login (insecure) with an API key, then dials with mTLS and
// the availability probe (DestroySession noop → NotFound) succeeds. This is
// the exact path `k8e sandbox status` exercises; a regression here means the
// gateway handshake (currently EOF on remote AWS) is broken server-side.
func TestTLSGatewayLoop(t *testing.T) {
	certDir := t.TempDir()
	o := newTestOrchestrator()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed the sandbox-apikeys Secret so Login can authenticate (legacy flat format).
	keysJSON := []byte(`{"tls-test":"tls-test-key"}`)
	if _, err := o.k8s.CoreV1().Secrets(apiKeySecretNS).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: apiKeySecretName, Namespace: apiKeySecretNS},
		Data:       map[string][]byte{"keys.json": keysJSON},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed api key secret: %v", err)
	}

	port := freePort(t)
	srv := NewServer(ServerConfig{
		K8s:               o.k8s,
		Dyn:               o.dynamic,
		CACertFile:        filepath.Join(certDir, "ca.crt"),
		CAKeyFile:         filepath.Join(certDir, "ca.key"),
		ServerCertFile:    filepath.Join(certDir, "server.crt"),
		ServerKeyFile:     filepath.Join(certDir, "server.key"),
		GRPCPort:          port,
		LocalAuth:         false,
		AdvertiseHostname: "localhost",
	})
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()
	waitForPort(t, port)
	defer func() {
		cancel()
		select {
		case <-startErr:
		case <-time.After(5 * time.Second):
			t.Error("gateway did not stop after cancel")
		}
	}()

	// Client bootstrap into an isolated cert dir (never touch ~/.k8e/sandbox).
	t.Setenv("K8E_SANDBOX_CERT_DIR", filepath.Join(certDir, "client"))
	c, err := client.NewClientWithEndpoint(fmt.Sprintf("127.0.0.1:%d", port), "tls-test-key")
	if err != nil {
		t.Fatalf("client bootstrap: %v", err)
	}
	defer c.Close()

	// Availability probe — the exact shape `k8e sandbox status` uses: the
	// gateway answers (handshake + auth complete) and the noop destroy fails
	// only because the session does not exist. The CLI treats the
	// "not found" text as a healthy gateway (errSessionNotFound).
	_, err = c.SandboxServiceClient.DestroySession(ctx, &pb.DestroySessionRequest{SessionId: "healthcheck-probe-noop"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("status probe: expected session-not-found response, got %v — gateway TLS handshake broken", err)
	}
}
