package grpc

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

// seedSession creates a SandboxSession CRD in the fake dynamic client.
func seedSession(t *testing.T, o *Orchestrator, id string) {
	t.Helper()
	sess := &sandboxv1.SandboxSession{}
	sess.Name = id
	sess.Namespace = sandboxNS
	u, err := sessionToUnstructured(sess)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if _, err := o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// TestExposeService_RegistersAndReturnsGatewayURL verifies the expose flow
// registers the port, returns the gateway URL, and re-applies the CNP with an
// ingress rule for the exposed port (gateway + e2b-server).
func TestExposeService_RegistersAndReturnsGatewayURL(t *testing.T) {
	o := newTestOrchestrator()
	seedSession(t, o, "sess-1")

	resp, err := o.ExposeService(context.Background(), "sess-1", 8080, "127.0.0.1", "http://gw.example.com")
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if want := "http://gw.example.com/k8e/expose/sess-1/8080/"; resp.Url != want {
		t.Fatalf("expose URL = %q, want %q", resp.Url, want)
	}

	// CNP must allow gateway + e2b-server ingress to :8080.
	obj, err := o.dynamic.Resource(cnpGVR).Namespace(sandboxNS).Get(context.Background(), "sandbox-session-sess-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("CNP not applied: %v", err)
	}
	spec := obj.Object["spec"].(map[string]interface{})
	ingress := spec["ingress"].([]interface{})
	seen8080 := 0
	for _, r := range ingress {
		rule := r.(map[string]interface{})
		for _, p := range rule["toPorts"].([]interface{}) {
			ports := p.(map[string]interface{})["ports"].([]interface{})
			if ports[0].(map[string]interface{})["port"] == "8080" {
				seen8080++
			}
		}
	}
	if seen8080 != 2 {
		t.Fatalf("expected 2 ingress rules for :8080 (gateway + e2b-server), got %d", seen8080)
	}
}

// TestExposeService_Idempotent verifies re-exposing an already-exposed port
// returns the existing URL without touching the CNP.
func TestExposeService_Idempotent(t *testing.T) {
	o := newTestOrchestrator()
	o.exposeMu.Lock()
	o.exposed["sess-1"] = []*ExposedEntry{
		{Port: 8080, Host: "127.0.0.1", URL: "http://gw/k8e/expose/sess-1/8080/", StartedAt: time.Now()},
	}
	o.exposeMu.Unlock()

	resp, err := o.ExposeService(context.Background(), "sess-1", 8080, "", "http://other")
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if resp.Url != "http://gw/k8e/expose/sess-1/8080/" {
		t.Fatalf("idempotent expose returned %q, want existing URL", resp.Url)
	}
}

// TestExposeService_InvalidPort verifies port bounds validation happens
// before any registry/CNP mutation.
func TestExposeService_InvalidPort(t *testing.T) {
	o := newTestOrchestrator()
	if _, err := o.ExposeService(context.Background(), "sess-1", 0, "", "http://gw"); err == nil {
		t.Fatal("expected error for port 0")
	}
	if _, err := o.ExposeService(context.Background(), "sess-1", 70000, "", "http://gw"); err == nil {
		t.Fatal("expected error for port 70000")
	}
}

// TestExposeService_UnknownSession verifies a missing session errors.
func TestExposeService_UnknownSession(t *testing.T) {
	o := newTestOrchestrator()
	if _, err := o.ExposeService(context.Background(), "nope", 8080, "", "http://gw"); err == nil {
		t.Fatal("expected error for unknown session")
	}
}

// TestUnexposeService_Idempotent verifies teardown removes the registry entry
// and the CNP loses the exposed port; a second call reports ok=false.
func TestUnexposeService_Idempotent(t *testing.T) {
	o := newTestOrchestrator()
	seedSession(t, o, "sess-1")
	o.exposeMu.Lock()
	o.exposed["sess-1"] = []*ExposedEntry{
		{Port: 8080, Host: "127.0.0.1", URL: "http://gw/k8e/expose/sess-1/8080/", StartedAt: time.Now()},
	}
	o.exposeMu.Unlock()

	resp, err := o.UnexposeService(context.Background(), "sess-1", 8080)
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if !resp.Ok {
		t.Fatal("expected ok=true on first unexpose")
	}
	o.exposeMu.Lock()
	remaining := len(o.exposed["sess-1"])
	o.exposeMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected registry entry removed, %d remain", remaining)
	}

	resp2, err := o.UnexposeService(context.Background(), "sess-1", 8080)
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if resp2.Ok {
		t.Fatal("expected ok=false on second unexpose (idempotent)")
	}
}

// TestListExposed_PrunesGoneSession verifies entries for a session that no
// longer exists are pruned.
func TestListExposed_PrunesGoneSession(t *testing.T) {
	o := newTestOrchestrator()
	o.exposeMu.Lock()
	o.exposed["gone"] = []*ExposedEntry{
		{Port: 8080, Host: "127.0.0.1", URL: "http://gw/k8e/expose/gone/8080/", StartedAt: time.Now()},
	}
	o.exposeMu.Unlock()

	resp, err := o.ListExposed(context.Background(), "gone")
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if len(resp.Services) != 0 {
		t.Fatalf("expected gone-session exposure pruned, got %d services", len(resp.Services))
	}
	o.exposeMu.Lock()
	_, still := o.exposed["gone"]
	o.exposeMu.Unlock()
	if still {
		t.Fatal("expected empty session removed from registry")
	}
}

// TestUpdateAllowedHosts_AppliesAndPersists verifies the allowlist update
// persists to the session spec and re-applies the CNP (live semantics).
func TestUpdateAllowedHosts_AppliesAndPersists(t *testing.T) {
	o := newTestOrchestrator()
	seedSession(t, o, "sess-1")

	hosts, err := o.UpdateAllowedHosts(context.Background(), "sess-1", []string{"a.com", "b.com"})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if len(hosts) != 2 || hosts[0] != "a.com" {
		t.Fatalf("unexpected returned hosts: %v", hosts)
	}

	got, err := o.getSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(got.Spec.AllowedHosts) != 2 || got.Spec.AllowedHosts[0] != "a.com" {
		t.Fatalf("session spec.allowedHosts not persisted: %v", got.Spec.AllowedHosts)
	}

	// CNP must have been (re)applied: the per-session policy exists.
	cnp, err := o.dynamic.Resource(cnpGVR).Namespace(sandboxNS).Get(context.Background(), "sandbox-session-sess-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("CNP not applied after allowlist update: %v", err)
	}
	if cnp.GetName() != "sandbox-session-sess-1" {
		t.Fatalf("unexpected CNP name %q", cnp.GetName())
	}

	// Clearing the list is allowed and persists.
	if _, err := o.UpdateAllowedHosts(context.Background(), "sess-1", nil); err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	got, err = o.getSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(got.Spec.AllowedHosts) != 0 {
		t.Fatalf("expected cleared allowedHosts, got %v", got.Spec.AllowedHosts)
	}
}

// TestUpdateAllowedHosts_UnknownSession verifies a missing session errors
// (NotFound) instead of silently succeeding.
func TestUpdateAllowedHosts_UnknownSession(t *testing.T) {
	o := newTestOrchestrator()
	if _, err := o.UpdateAllowedHosts(context.Background(), "nope", []string{"a.com"}); err == nil {
		t.Fatal("expected error for unknown session")
	}
}

// TestBuildSessionCNPExposed_Ingress verifies the CNP builder adds exactly one
// gateway + one e2b-server ingress rule per exposed port, and keeps :2024.
func TestBuildSessionCNPExposed_Ingress(t *testing.T) {
	sess := &sandboxv1.SandboxSession{}
	sess.Name = "cnp-exposed"
	sess.Namespace = sandboxNS

	obj := buildSessionCNPExposed(sess, false, []int32{8080, 9090})
	spec := obj.Object["spec"].(map[string]interface{})
	ingress := spec["ingress"].([]interface{})

	portCount := map[string]int{}
	for _, r := range ingress {
		rule := r.(map[string]interface{})
		for _, p := range rule["toPorts"].([]interface{}) {
			ports := p.(map[string]interface{})["ports"].([]interface{})
			portCount[ports[0].(map[string]interface{})["port"].(string)]++
		}
	}
	if portCount["2024"] != 3 { // host + gateway + e2b-server
		t.Fatalf("expected 3 ingress rules for :2024, got %d", portCount["2024"])
	}
	if portCount["8080"] != 2 || portCount["9090"] != 2 {
		t.Fatalf("expected 2 ingress rules per exposed port, got %v", portCount)
	}
	// No exposure → same shape as before.
	obj2 := buildSessionCNPExposed(sess, false, nil)
	ingress2 := obj2.Object["spec"].(map[string]interface{})["ingress"].([]interface{})
	if len(ingress2) != 3 {
		t.Fatalf("expected 3 ingress rules without exposure, got %d", len(ingress2))
	}
}
