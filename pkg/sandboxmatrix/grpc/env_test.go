package grpc

import (
	"context"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

func TestValidateSessionEnv(t *testing.T) {
	tooMany := make(map[string]string, maxSessionEnvKeys+1)
	for i := 0; i < maxSessionEnvKeys+1; i++ {
		tooMany["k"+strconv.Itoa(i)] = "v"
	}
	n := maxSessionEnvTotalBytes/maxSessionEnvValueBytes + 1
	if n > maxSessionEnvKeys {
		n = maxSessionEnvKeys
	}
	tooBigTotal := make(map[string]string, n)
	for i := 0; i < n; i++ {
		tooBigTotal["k"+strconv.Itoa(i)] = strings.Repeat("v", maxSessionEnvValueBytes)
	}

	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "nil", env: nil},
		{name: "empty", env: map[string]string{}},
		{name: "ok", env: map[string]string{"FOO": "bar", "BAZ": "qux=with=equals"}},
		{name: "too many keys", env: tooMany, wantErr: "too many entries"},
		{name: "value too large", env: map[string]string{"BIG": strings.Repeat("a", maxSessionEnvValueBytes+1)}, wantErr: "value too long"},
		{name: "key too large", env: map[string]string{strings.Repeat("k", maxSessionEnvKeyBytes+1): "v"}, wantErr: "too long"},
		{name: "total too large", env: tooBigTotal, wantErr: "total size exceeds"},
		{name: "empty key", env: map[string]string{"": "x"}, wantErr: "empty key"},
		{name: "key equals", env: map[string]string{"FOO=BAR": "x"}, wantErr: "'=' not allowed"},
		{name: "nul value", env: map[string]string{"FOO": "a\x00b"}, wantErr: "NUL byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSessionEnv(tc.env)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestSandboxdRequestBodies(t *testing.T) {
	env := map[string]string{"FOO": "bar", "BAZ": "qux"}

	empty := sandboxdExecBody("echo hi", 30, "/workspace", nil)
	if _, ok := empty["env"]; ok {
		t.Fatalf("env should be omitted when empty: %v", empty)
	}
	if empty["command"] != "echo hi" || empty["timeout"] != int32(30) || empty["workdir"] != "/workspace" {
		t.Fatalf("unexpected exec body: %v", empty)
	}

	withEnv := sandboxdExecBody("true", 10, "/tmp", env)
	got, ok := withEnv["env"].(map[string]string)
	if !ok || got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("exec env payload: %v", withEnv["env"])
	}

	bg := sandboxdBackgroundBody("sleep 1", "run-1", 60, "/workspace", map[string]string{"FOO": "bar"})
	if bg["run_id"] != "run-1" {
		t.Fatalf("run_id: %v", bg["run_id"])
	}
	got, ok = bg["env"].(map[string]string)
	if !ok || got["FOO"] != "bar" {
		t.Fatalf("background env: %v", bg["env"])
	}
}

func TestGetSessionEnv_ReturnsPersistedEnv(t *testing.T) {
	s := newTestServer()
	sess, err := s.orch.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "env-get-test",
		Env:       map[string]string{"FOO": "bar", "BAZ": "qux"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := s.getSessionEnv(context.Background(), sess.Name)
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("getSessionEnv: %v", got)
	}
}

func TestGetSessionEnv_MissingSession(t *testing.T) {
	s := newTestServer()
	if got := s.getSessionEnv(context.Background(), "does-not-exist"); got != nil {
		t.Fatalf("expected nil on missing session, got %v", got)
	}
}

func TestGetSessionEnv_NoEnvField(t *testing.T) {
	s := newTestServer()
	sess, err := s.orch.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "env-none"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := s.getSessionEnv(context.Background(), sess.Name); got != nil {
		t.Fatalf("expected nil when no env, got %v", got)
	}
}

func TestCreateSession_RejectsOversizedEnv(t *testing.T) {
	big := map[string]string{"BIG": strings.Repeat("x", maxSessionEnvValueBytes+1)}
	if err := validateSessionEnv(big); err == nil {
		t.Fatal("precondition: validate should fail")
	}
	_, err := newTestOrchestrator().CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "env-too-big",
		Env:       big,
	})
	if err == nil {
		t.Fatal("expected CreateSession to reject oversized env")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetSessionEnv_UnreadableEnvField(t *testing.T) {
	s := newTestServer()
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testAPIVer,
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "env-bad", "namespace": sandboxNS},
		"spec": map[string]interface{}{
			"runtimeClass": "gvisor",
			"env":          "not-a-map",
		},
		"status": map[string]interface{}{"phase": "Active", "podIP": "10.0.0.1"},
	}}
	if _, err := s.dyn.Resource(sessionGVR).Namespace(sandboxNS).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := s.getSessionEnv(context.Background(), "env-bad"); got != nil {
		t.Fatalf("expected nil on unreadable env, got %v", got)
	}
}

// newTestServer reuses the shared fake k8s/dynamic clients from newTestOrchestrator
// (avoids duplicating the scheme/listKinds setup that Sonar flagged).
func newTestServer() *Server {
	o := newTestOrchestrator()
	return &Server{k8s: o.k8s, dyn: o.dynamic, orch: o}
}

func TestClassifyExecStatus(t *testing.T) {
	if got := classifyExecStatus(false, false); got != execStatusCompleted {
		t.Fatalf("got %s", got)
	}
	if got := classifyExecStatus(true, false); got != execStatusTimedOut {
		t.Fatalf("got %s", got)
	}
	if got := classifyExecStatus(false, true); got != execStatusFailed {
		t.Fatalf("got %s", got)
	}
}

func TestValidateSecretRefs(t *testing.T) {
	if err := validateSecretRefs(nil); err != nil {
		t.Fatal(err)
	}
	if err := validateSecretRefs([]*pb.SecretRef{{SecretName: "s", Key: "k", EnvVar: "API_KEY"}}); err != nil {
		t.Fatal(err)
	}
	if err := validateSecretRefs([]*pb.SecretRef{{SecretName: "s", Key: "k"}}); err == nil {
		t.Fatal("expected missing env_var error")
	}
	if err := validateSecretRefs([]*pb.SecretRef{
		{SecretName: "s", Key: "k", EnvVar: "A"},
		{SecretName: "s", Key: "k2", EnvVar: "A"},
	}); err == nil {
		t.Fatal("expected duplicate env_var")
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := newTestServer()
	_, err := s.GetSession(context.Background(), &pb.GetSessionRequest{SessionId: "missing"})
	if err == nil {
		t.Fatal("expected not found")
	}
}

func TestGetSession_AndListSessions(t *testing.T) {
	s := newTestServer()
	sess, err := s.orch.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "get-1",
		TenantId:  "t1",
		Env:       map[string]string{"FOO": "bar"},
		SecretRefs: []*pb.SecretRef{
			{SecretName: "llm", Key: "token", EnvVar: "API_KEY"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	view, err := s.GetSession(context.Background(), &pb.GetSessionRequest{SessionId: sess.Name})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view.Phase == "" || view.RuntimeClass == "" {
		t.Fatalf("view incomplete: %+v", view)
	}
	if len(view.EnvKeys) != 1 || view.EnvKeys[0] != "FOO" {
		t.Fatalf("env_keys: %v", view.EnvKeys)
	}
	if len(view.SecretEnvVars) != 1 || view.SecretEnvVars[0] != "API_KEY" {
		t.Fatalf("secret_env_vars: %v", view.SecretEnvVars)
	}
	// Ensure secret values never appear in the view payload shape.
	if strings.Contains(view.String(), "bar") {
		t.Fatal("env value leaked into proto String()")
	}

	list, err := s.ListSessions(context.Background(), &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Sessions) < 1 {
		t.Fatalf("expected sessions, got %d", len(list.Sessions))
	}
}

func TestResolveSessionEnv_WithSecret(t *testing.T) {
	s := newTestServer()
	// Seed K8s secret.
	_, err := s.k8s.CoreV1().Secrets(sandboxNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm", Namespace: sandboxNS},
		Data:       map[string][]byte{"token": []byte("super-secret")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	_, err = s.orch.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "sec-1",
		Env:       map[string]string{"LOG": "1"},
		SecretRefs: []*pb.SecretRef{
			{SecretName: "llm", Key: "token", EnvVar: "API_KEY"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	env, err := s.resolveSessionEnv(context.Background(), "sec-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env["LOG"] != "1" || env["API_KEY"] != "super-secret" {
		t.Fatalf("merged env: %v", env)
	}
}

func TestResolveSessionEnv_MissingSecretFails(t *testing.T) {
	s := newTestServer()
	_, err := s.orch.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "sec-miss",
		SecretRefs: []*pb.SecretRef{
			{SecretName: "nope", Key: "k", EnvVar: "X"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = s.resolveSessionEnv(context.Background(), "sec-miss")
	if err == nil {
		t.Fatal("expected secret resolution error")
	}
}