package e2b

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// TestControlPlaneTenantIsolation verifies that a control-plane API key bound
// to a tenant can only reach its own sandboxes, that create stamps the caller's
// tenant, and that list is filtered. An unbound key keeps full access, so the
// legacy shared-credential model is unaffected.
func TestControlPlaneTenantIsolation(t *testing.T) {
	gw := newFakeGateway()
	gw.sessions["other-sandbox"] = &pb.GetSessionResponse{
		SessionId: "other-sandbox", Phase: "Active", RuntimeClass: "gvisor", TenantId: "other",
	}

	s := NewServer(Config{SigningSecret: "test-secret"}, gw)
	s.ReplaceAPIKeysWithTenants([]string{"token"}, map[string]string{"token": "acme"})
	ts := httptest.NewServer(s.Handle())
	defer ts.Close()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-API-KEY", "token")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		s.Handle().ServeHTTP(rec, req)
		return rec
	}

	// Cross-tenant GET is indistinguishable from a missing sandbox.
	if rec := do(http.MethodGet, "/sandboxes/other-sandbox", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get: got %d, want 404", rec.Code)
	}
	// Cross-tenant DELETE must not destroy it.
	if rec := do(http.MethodDelete, "/sandboxes/other-sandbox", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: got %d, want 404", rec.Code)
	}
	if _, ok := gw.sessions["other-sandbox"]; !ok {
		t.Fatal("cross-tenant delete must not destroy the other tenant's sandbox")
	}

	// Create stamps the caller's tenant on the session.
	if rec := do(http.MethodPost, "/sandboxes", `{"templateID":"base"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(gw.created) != 1 || gw.created[0].TenantId != "acme" {
		t.Fatalf("created sessions: %+v", gw.created)
	}

	// List only returns the caller's sessions.
	rec := do(http.MethodGet, "/v2/sandboxes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "other-sandbox") {
		t.Fatalf("list leaked another tenant's sandbox: %s", rec.Body.String())
	}

	// An unbound key keeps full access (legacy shared-credential model).
	legacy := NewServer(Config{SigningSecret: "test-secret"}, gw)
	legacy.ReplaceAPIKeys([]string{"legacy"})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sandboxes/other-sandbox", nil)
	req.Header.Set("X-API-KEY", "legacy")
	rec = httptest.NewRecorder()
	legacy.Handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbound key: got %d, want 200", rec.Code)
	}
}

// TestControlPlaneTenantNameConflict verifies that a tenant cannot reacquire a
// metadata.name that belongs to another tenant.
func TestControlPlaneTenantNameConflict(t *testing.T) {
	gw := newFakeGateway()
	gw.sessions["shared-name"] = &pb.GetSessionResponse{
		SessionId: "shared-name", Phase: "Active", RuntimeClass: "gvisor", TenantId: "other",
	}

	s := NewServer(Config{SigningSecret: "test-secret"}, gw)
	s.registry.put(&registryEntry{sandboxID: "shared-name", metadata: map[string]string{"name": "shared-name"}})
	s.ReplaceAPIKeysWithTenants([]string{"token"}, map[string]string{"token": "acme"})
	ts := httptest.NewServer(s.Handle())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes", strings.NewReader(`{"templateID":"base","metadata":{"name":"shared-name"}}`))
	req.Header.Set("X-API-KEY", "token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cross-tenant name reuse: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}
