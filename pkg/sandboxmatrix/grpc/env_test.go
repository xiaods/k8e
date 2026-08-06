package grpc

import (
	"context"
	"strconv"
	"strings"
	"testing"

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