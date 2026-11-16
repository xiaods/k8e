package grpc

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// seedSecret creates a Secret in the sandbox namespace for secret_refs tests.
func seedSecret(t *testing.T, s *Server, name string, labels map[string]string, key, value string) {
	t.Helper()
	_, err := s.k8s.CoreV1().Secrets(sandboxNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sandboxNS, Labels: labels},
		Data:       map[string][]byte{key: []byte(value)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed secret %s: %v", name, err)
	}
}

func TestCheckSecretTenantScope(t *testing.T) {
	cases := []struct {
		name    string
		labels  map[string]string
		tenant  string
		wantErr bool
	}{
		{"unlabelled default is legacy-allowed", nil, defaultTenantID, false},
		{"unlabelled empty tenant is default", nil, "", false},
		{"unlabelled non-default rejected", nil, "acme", true},
		{"matching label allowed", map[string]string{secretTenantLabel: "acme"}, "acme", false},
		{"mismatched label rejected", map[string]string{secretTenantLabel: "acme"}, "other", true},
		{"labelled secret not readable by default", map[string]string{secretTenantLabel: "acme"}, defaultTenantID, true},
		{"blank label treated as unlabelled", map[string]string{secretTenantLabel: "  "}, defaultTenantID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSecretTenantScope("s", tc.labels, tc.tenant)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestReadSecretKey_TenantScope(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()

	seedSecret(t, s, "acme-secret", map[string]string{secretTenantLabel: "acme"}, "token", "acme-value")
	seedSecret(t, s, "legacy-secret", nil, "token", "legacy-value")

	if got, err := s.readSecretKey(ctx, "acme-secret", "token", "acme"); err != nil || got != "acme-value" {
		t.Fatalf("owner tenant: got %q, %v", got, err)
	}
	if _, err := s.readSecretKey(ctx, "acme-secret", "token", "other"); err == nil {
		t.Fatal("a different tenant must not read an acme-labelled secret")
	}
	if _, err := s.readSecretKey(ctx, "acme-secret", "token", defaultTenantID); err == nil {
		t.Fatal("the default tenant must not read a tenant-labelled secret")
	}

	if got, err := s.readSecretKey(ctx, "legacy-secret", "token", ""); err != nil || got != "legacy-value" {
		t.Fatalf("legacy default: got %q, %v", got, err)
	}
	if _, err := s.readSecretKey(ctx, "legacy-secret", "token", "acme"); err == nil {
		t.Fatal("a non-default tenant must not read an unlabelled secret")
	}
}

// TestResolveSessionEnv_RejectsCrossTenantSecret is the end-to-end regression:
// a session owned by one tenant must not be able to pull another tenant's
// Secret into its environment, and the error must not echo the value.
func TestResolveSessionEnv_RejectsCrossTenantSecret(t *testing.T) {
	s := newTestServer()
	ctx := context.Background()

	seedSecret(t, s, "victim-secret", map[string]string{secretTenantLabel: "victim"}, "token", "do-not-leak")

	if _, err := s.orch.CreateSession(ctx, &pb.CreateSessionRequest{
		SessionId: "attacker-session",
		TenantId:  "attacker",
		SecretRefs: []*pb.SecretRef{
			{SecretName: "victim-secret", Key: "token", EnvVar: "STOLEN"},
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	env, err := s.resolveSessionEnv(ctx, "attacker-session")
	if err == nil {
		t.Fatal("cross-tenant secret_ref must fail")
	}
	if env != nil {
		t.Fatalf("no env must be returned on failure, got %v", env)
	}
	if strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("error leaked the secret value: %v", err)
	}
}
