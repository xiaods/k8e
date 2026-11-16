package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// certContext builds a context whose peer presents a client certificate with
// the given CommonName, as the mTLS transport would.
func certContext(cn string) context.Context {
	cert := &x509.Certificate{
		Subject:   pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
	}
	ai := credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}}
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 50051},
		AuthInfo: ai,
	})
}

func TestCallerTenantFromCertificate(t *testing.T) {
	s := &Server{}
	s.replaceAPIKeysAndTenants(
		map[string]string{"agent-a": "secret-a", "agent-b": "secret-b"},
		map[string]string{"agent-a": "acme"},
	)

	tenant, ok := s.callerTenant(certContext("agent-a"))
	if !ok || tenant != "acme" {
		t.Fatalf("bound key: got tenant %q, ok=%v; want acme,true", tenant, ok)
	}

	// A key with no tenant binding must not constrain the caller.
	if tenant, ok := s.callerTenant(certContext("agent-b")); ok {
		t.Fatalf("unbound key: got tenant %q, ok=true; want ok=false", tenant)
	}

	// A loopback peer (the embedded e2b server) has no certificate and no
	// tenant; it keeps full access.
	loopback := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 50051},
	})
	if tenant, ok := s.callerTenant(loopback); ok {
		t.Fatalf("loopback: got tenant %q, ok=true; want ok=false", tenant)
	}

	// The interceptor stashes the binding in the context for the handlers.
	ctx := s.withCallerTenant(certContext("agent-a"))
	if got, ok := callerTenantFromContext(ctx); !ok || got != "acme" {
		t.Fatalf("context tenant: got %q, ok=%v; want acme,true", got, ok)
	}
	if _, ok := callerTenantFromContext(context.Background()); ok {
		t.Fatal("no binding must report ok=false")
	}
}

func TestGetSessionTenantIsolation(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	if _, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{SessionId: "s-acme", TenantId: "acme"}, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := o.getSession(contextWithCallerTenant(ctx, "acme"), "s-acme"); err != nil {
		t.Fatalf("same tenant must be allowed: %v", err)
	}
	// A different tenant gets NotFound, not PermissionDenied: it must not learn
	// that the session exists.
	if _, err := o.getSession(contextWithCallerTenant(ctx, "other"), "s-acme"); status.Code(err) != codes.NotFound {
		t.Fatalf("cross tenant: got %v, want NotFound", err)
	}
	// An unbound caller (loopback, or a key with no tenant) keeps full access.
	if _, err := o.getSession(ctx, "s-acme"); err != nil {
		t.Fatalf("unbound caller must be allowed: %v", err)
	}
}

func TestListSessionsTenantFilter(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	for _, s := range []struct{ id, tenant string }{
		{"s-acme", "acme"},
		{"s-other", "other"},
	} {
		if _, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{SessionId: s.id, TenantId: s.tenant}, 0); err != nil {
			t.Fatalf("create %s: %v", s.id, err)
		}
	}

	all, err := o.listSessions(ctx, sandboxNS, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unbound caller: got %d sessions, want 2", len(all))
	}

	scoped, err := o.listSessions(contextWithCallerTenant(ctx, "acme"), sandboxNS, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Name != "s-acme" {
		t.Fatalf("scoped caller: got %+v, want only s-acme", scoped)
	}
}

func TestCreateSessionTenantBinding(t *testing.T) {
	o := newTestOrchestrator()
	ctx := contextWithCallerTenant(context.Background(), "acme")

	// Omitted tenant: the bound tenant is applied.
	s, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{}, 0)
	if err != nil {
		t.Fatalf("create with omitted tenant: %v", err)
	}
	if s.Spec.TenantID != "acme" {
		t.Fatalf("session tenant: got %q, want acme", s.Spec.TenantID)
	}

	// Matching tenant: allowed.
	if _, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{TenantId: "acme"}, 0); err != nil {
		t.Fatalf("create with matching tenant: %v", err)
	}

	// Different tenant: rejected, so the caller cannot file a session under a
	// tenant it cannot see.
	if _, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{TenantId: "other"}, 0); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("create with mismatched tenant: got %v, want PermissionDenied", err)
	}
}

func TestExposeServiceTenantIsolation(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	if _, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{SessionId: "s-acme", TenantId: "acme"}, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	acme := contextWithCallerTenant(ctx, "acme")
	resp, err := o.ExposeService(acme, "s-acme", 8080, "127.0.0.1", "http://gw.example.com")
	if err != nil {
		t.Fatalf("expose: %v", err)
	}
	if resp.Url == "" {
		t.Fatal("expected a URL")
	}

	// The idempotent fast path must enforce the tenant too: without the check a
	// caller from another tenant could retrieve the existing URL by session ID
	// and port.
	other := contextWithCallerTenant(ctx, "other")
	if _, err := o.ExposeService(other, "s-acme", 8080, "127.0.0.1", "http://gw.example.com"); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant idempotent expose: got %v, want NotFound", err)
	}
	// And it must not be able to list or clear the exposure.
	if list, err := o.ListExposed(other, "s-acme"); err != nil || len(list.Services) != 0 {
		t.Fatalf("cross-tenant list: got %+v, %v; want empty", list, err)
	}
	if unexpose, err := o.UnexposeService(other, "s-acme", 8080); err != nil || unexpose.Ok {
		t.Fatalf("cross-tenant unexpose: got %+v, %v; want ok=false", unexpose, err)
	}
	// The owner can still see it.
	if list, err := o.ListExposed(acme, "s-acme"); err != nil || len(list.Services) != 1 {
		t.Fatalf("owner list: got %+v, %v; want 1 service", list, err)
	}
}

func TestConfirmActionApprovalIDIsRandom(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	if _, err := o.CreateSessionWithTTL(ctx, &pb.CreateSessionRequest{SessionId: "s-1"}, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	a, err := o.ConfirmAction(ctx, &pb.ConfirmActionRequest{SessionId: "s-1", Action: "deploy"})
	if err != nil {
		t.Fatalf("confirm 1: %v", err)
	}
	b, err := o.ConfirmAction(ctx, &pb.ConfirmActionRequest{SessionId: "s-1", Action: "deploy"})
	if err != nil {
		t.Fatalf("confirm 2: %v", err)
	}
	if a.ApprovalId == b.ApprovalId {
		t.Fatalf("approval IDs must be unique, both were %q", a.ApprovalId)
	}
	if len(a.ApprovalId) <= len("approval-") {
		t.Fatalf("approval ID %q is too short to be a capability", a.ApprovalId)
	}
}
