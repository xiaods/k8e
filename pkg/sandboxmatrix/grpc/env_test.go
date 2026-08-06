package grpc

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

func TestValidateSessionEnv_EmptyOK(t *testing.T) {
	if err := validateSessionEnv(nil); err != nil {
		t.Fatalf("nil env: %v", err)
	}
	if err := validateSessionEnv(map[string]string{}); err != nil {
		t.Fatalf("empty env: %v", err)
	}
}

func TestValidateSessionEnv_OK(t *testing.T) {
	err := validateSessionEnv(map[string]string{
		"FOO": "bar",
		"BAZ": "qux=with=equals",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidateSessionEnv_TooManyKeys(t *testing.T) {
	env := make(map[string]string, maxSessionEnvKeys+1)
	for i := 0; i < maxSessionEnvKeys+1; i++ {
		env["k"+itoa(i)] = "v"
	}
	if err := validateSessionEnv(env); err == nil {
		t.Fatal("expected too many keys error")
	}
}

func TestValidateSessionEnv_ValueTooLarge(t *testing.T) {
	err := validateSessionEnv(map[string]string{
		"BIG": strings.Repeat("a", maxSessionEnvValueBytes+1),
	})
	if err == nil {
		t.Fatal("expected value too long")
	}
}

func TestValidateSessionEnv_KeyTooLarge(t *testing.T) {
	err := validateSessionEnv(map[string]string{
		strings.Repeat("k", maxSessionEnvKeyBytes+1): "v",
	})
	if err == nil {
		t.Fatal("expected key too long")
	}
}

func TestValidateSessionEnv_TotalTooLarge(t *testing.T) {
	// Each value max-sized; fewer than max keys but total over limit.
	n := maxSessionEnvTotalBytes/maxSessionEnvValueBytes + 1
	if n > maxSessionEnvKeys {
		n = maxSessionEnvKeys
	}
	env := make(map[string]string, n)
	for i := 0; i < n; i++ {
		env["k"+itoa(i)] = strings.Repeat("v", maxSessionEnvValueBytes)
	}
	if err := validateSessionEnv(env); err == nil {
		t.Fatal("expected total size error")
	}
}

func TestValidateSessionEnv_EmptyKey(t *testing.T) {
	if err := validateSessionEnv(map[string]string{"": "x"}); err == nil {
		t.Fatal("expected empty key error")
	}
}

func TestValidateSessionEnv_KeyContainsEquals(t *testing.T) {
	if err := validateSessionEnv(map[string]string{"FOO=BAR": "x"}); err == nil {
		t.Fatal("expected '=' in key error")
	}
}

func TestValidateSessionEnv_NUL(t *testing.T) {
	if err := validateSessionEnv(map[string]string{"FOO": "a\x00b"}); err == nil {
		t.Fatal("expected NUL error")
	}
}

func TestSandboxdExecBody_OmitsEmptyEnv(t *testing.T) {
	body := sandboxdExecBody("echo hi", 30, "/workspace", nil)
	if _, ok := body["env"]; ok {
		t.Fatalf("env should be omitted when empty: %v", body)
	}
	if body["command"] != "echo hi" || body["timeout"] != int32(30) || body["workdir"] != "/workspace" {
		t.Fatalf("unexpected body: %v", body)
	}
}

func TestSandboxdExecBody_IncludesEnv(t *testing.T) {
	env := map[string]string{"FOO": "bar", "BAZ": "qux"}
	body := sandboxdExecBody("true", 10, "/tmp", env)
	got, ok := body["env"].(map[string]string)
	if !ok {
		t.Fatalf("env type: %T", body["env"])
	}
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("env payload: %v", got)
	}
}

func TestSandboxdBackgroundBody_IncludesEnv(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	body := sandboxdBackgroundBody("sleep 1", "run-1", 60, "/workspace", env)
	if body["run_id"] != "run-1" {
		t.Fatalf("run_id: %v", body["run_id"])
	}
	got, ok := body["env"].(map[string]string)
	if !ok || got["FOO"] != "bar" {
		t.Fatalf("env: %v", body["env"])
	}
}

func TestGetSessionEnv_ReturnsPersistedEnv(t *testing.T) {
	s := newTestServer()
	// Create via orchestrator (bypasses capacity) with env.
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
	got := s.getSessionEnv(context.Background(), "does-not-exist")
	if got != nil {
		t.Fatalf("expected nil on missing session, got %v", got)
	}
}

func TestGetSessionEnv_NoEnvField(t *testing.T) {
	s := newTestServer()
	sess, err := s.orch.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "env-none",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := s.getSessionEnv(context.Background(), sess.Name)
	if got != nil {
		t.Fatalf("expected nil when no env, got %v", got)
	}
}

func TestCreateSession_RejectsOversizedEnv(t *testing.T) {
	o := newTestOrchestrator()
	err := validateSessionEnv(map[string]string{
		"BIG": strings.Repeat("x", maxSessionEnvValueBytes+1),
	})
	if err == nil {
		t.Fatal("precondition: validate should fail")
	}
	// Orchestrator path also enforces limits.
	_, err = o.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "env-too-big",
		Env:       map[string]string{"BIG": strings.Repeat("x", maxSessionEnvValueBytes+1)},
	})
	if err == nil {
		t.Fatal("expected CreateSession to reject oversized env")
	}
	if st, ok := statusFrom(err); !ok || st != "InvalidArgument" {
		// Accept any error that mentions env; status code when available.
		if !strings.Contains(err.Error(), "env") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestGetSessionEnv_UnreadableEnvField(t *testing.T) {
	s := newTestServer()
	// Seed a session object where spec.env is not a string map.
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
	_, err := s.dyn.Resource(sessionGVR).Namespace(sandboxNS).Create(context.Background(), u, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := s.getSessionEnv(context.Background(), "env-bad")
	if got != nil {
		t.Fatalf("expected nil on unreadable env, got %v", got)
	}
}

func newTestServer() *Server {
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{
		{Group: testGroupK8e, Version: "v1alpha1", Kind: "SandboxSession"},
		{Group: testGroupK8e, Version: "v1alpha1", Kind: "SandboxMatrix"},
		{Group: testGroupCilium, Version: "v2", Kind: "CiliumNetworkPolicy"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: testGroupK8e, Version: "v1alpha1", Kind: "SandboxSessionList"},
		{Group: testGroupK8e, Version: "v1alpha1", Kind: "SandboxMatrixList"},
		{Group: testGroupCilium, Version: "v2", Kind: "CiliumNetworkPolicyList"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
	}
	listKinds := map[schema.GroupVersionResource]string{
		{Group: testGroupK8e, Version: "v1alpha1", Resource: "sandboxsessions"}:       "SandboxSessionList",
		{Group: testGroupK8e, Version: "v1alpha1", Resource: "sandboxmatrices"}:       "SandboxMatrixList",
		{Group: testGroupCilium, Version: "v2", Resource: "ciliumnetworkpolicies"}: "CiliumNetworkPolicyList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	k8s := kubefake.NewSimpleClientset()
	return NewServer(ServerConfig{K8s: k8s, Dyn: dyn})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [16]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func statusFrom(err error) (string, bool) {
	// Avoid importing status codes comparison noise; Error() from gRPC status embeds the code name.
	msg := err.Error()
	if strings.Contains(msg, "InvalidArgument") || strings.Contains(msg, "invalid argument") {
		return "InvalidArgument", true
	}
	return "", false
}
