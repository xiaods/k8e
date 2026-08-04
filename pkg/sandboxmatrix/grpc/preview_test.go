package grpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newTestPreviewOrchestrator returns an orchestrator with a preview engine
// wired up (ephemeral key, test config) plus a helper to re-read a session.
func newTestPreviewOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()
	o := newTestOrchestrator()
	cfg := PreviewConfig{
		Domain:        "preview.test.local",
		IngressClass:  "nginx",
		VerifyBaseURL: "http://127.0.0.1:50052",
	}
	if err := o.InitPreview(cfg, ""); err != nil {
		t.Fatalf("init preview: %v", err)
	}
	return o
}

// mustExpose exposes a port and fails the test on error.
func mustExpose(t *testing.T, o *Orchestrator, sid string, port int32, ttl int32) (string, int64) {
	t.Helper()
	url, exp, err := o.ExposePort(context.Background(), sid, port, ttl)
	if err != nil {
		t.Fatalf("expose: %v", err)
	}
	return url, exp
}

func TestPreviewToken_RoundTrip(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	tok, err := o.preview.mintToken("sess-abc", 8080, 3600)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parsed, err := o.preview.verifyToken(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if parsed.SessionID != "sess-abc" || parsed.Port != 8080 {
		t.Fatalf("unexpected token payload: %+v", parsed)
	}
	if parsed.ExpiresAt <= time.Now().Unix() {
		t.Fatal("expected future expiry")
	}
}

func TestPreviewToken_Expired(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	tok, err := o.preview.mintToken("sess-abc", 8080, -10)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := o.preview.verifyToken(tok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestPreviewToken_Tampered(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	tok, err := o.preview.mintToken("sess-abc", 8080, 3600)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// flip the last character of the signature half
	if tok[len(tok)-1] == 'A' {
		tok = tok[:len(tok)-1] + "B"
	} else {
		tok = tok[:len(tok)-1] + "A"
	}
	if _, err := o.preview.verifyToken(tok); err == nil {
		t.Fatal("expected tampered token to be rejected")
	}
}

func TestPreviewPath_Parse(t *testing.T) {
	cases := []struct {
		path string
		sid  string
		port int32
		tok  string
		ok   bool
	}{
		{"/p/sess-abc/8080/abcd1234/", "sess-abc", 8080, "abcd1234", true},
		{"/p/sess-abc/8080/abcd1234/sub/path", "sess-abc", 8080, "abcd1234", true},
		{"/p/sess-abc/8080/abcd1234", "sess-abc", 8080, "abcd1234", true},
		{"/p/sess-abc/notaport/tok/", "", 0, "", false},
		{"/other/sess-abc/8080/tok/", "", 0, "", false},
		{"/p/sess-abc/99999/tok/", "", 0, "", false},
	}
	for _, c := range cases {
		sid, port, tok, ok := parsePreviewPath(c.path)
		if ok != c.ok || sid != c.sid || port != c.port || tok != c.tok {
			t.Errorf("parsePreviewPath(%q) = (%q,%d,%q,%v), want (%q,%d,%q,%v)",
				c.path, sid, port, tok, ok, c.sid, c.port, c.tok, c.ok)
		}
	}
}

func TestPreviewService_SelectsSessionPod(t *testing.T) {
	svc := previewService("preview-sess-1-8080", "sess-1", 8080)
	if got := svc.Spec.Selector[labelSessionID]; got != "sess-1" {
		t.Fatalf("expected selector %s=sess-1, got %q", labelSessionID, got)
	}
	if len(svc.Spec.Selector) != 1 {
		t.Fatalf("expected selector to pin exactly one label, got %v", svc.Spec.Selector)
	}
	if svc.Spec.Ports[0].Port != 8080 || svc.Spec.Ports[0].TargetPort.IntValue() != 8080 {
		t.Fatalf("unexpected service port: %+v", svc.Spec.Ports[0])
	}
	if svc.Labels[previewLabel] != previewLabelValue {
		t.Fatal("expected preview label on Service for GC cleanup")
	}
}

func TestExposePort_CreatesRouteAndToken(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	ctx := context.Background()
	sess := mustCreateSession(t, o, "expose-1")
	url, exp := mustExpose(t, o, sess.Name, 8080, 60)

	if !strings.HasPrefix(url, "https://preview.test.local/p/expose-1/8080/") {
		t.Fatalf("unexpected preview URL: %s", url)
	}
	if exp <= time.Now().Unix() || exp > time.Now().Add(2*time.Minute).Unix() {
		t.Fatalf("unexpected expiry %d", exp)
	}

	name := previewResourceName(sess.Name, 8080)
	if _, err := o.k8s.CoreV1().Services(sandboxNS).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("preview Service not created: %v", err)
	}
	ing, err := o.k8s.NetworkingV1().Ingresses(sandboxNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("preview Ingress not created: %v", err)
	}
	if ing.Labels[labelSessionID] != sess.Name {
		t.Fatalf("Ingress missing session label: %v", ing.Labels)
	}
	if ing.Spec.Rules[0].Host != "preview.test.local" {
		t.Fatalf("unexpected ingress host: %s", ing.Spec.Rules[0].Host)
	}
	if got := ing.Annotations["nginx.ingress.kubernetes.io/auth-url"]; got != "http://127.0.0.1:50052/preview/verify" {
		t.Fatalf("unexpected auth-url: %s", got)
	}

	// session status records the exposed port
	got, err := o.getSession(ctx, sess.Name)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(got.Status.ExposedPorts) != 1 || got.Status.ExposedPorts[0].Port != 8080 {
		t.Fatalf("expected exposedPorts=[8080], got %+v", got.Status.ExposedPorts)
	}
}

func TestExposePort_ReexposeReusesRouteAndFreshToken(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	ctx := context.Background()
	sess := mustCreateSession(t, o, "expose-re")
	url1, exp1 := mustExpose(t, o, sess.Name, 8080, 3600)
	url2, exp2 := mustExpose(t, o, sess.Name, 8080, 3600)

	name := previewResourceName(sess.Name, 8080)
	// route reused, not recreated
	svcs, _ := o.k8s.CoreV1().Services(sandboxNS).List(ctx, metav1.ListOptions{LabelSelector: previewLabel + "=" + previewLabelValue})
	if len(svcs.Items) != 1 {
		t.Fatalf("expected exactly 1 preview Service after re-expose, got %d", len(svcs.Items))
	}
	ings, _ := o.k8s.NetworkingV1().Ingresses(sandboxNS).List(ctx, metav1.ListOptions{LabelSelector: previewLabel + "=" + previewLabelValue})
	if len(ings.Items) != 1 {
		t.Fatalf("expected exactly 1 preview Ingress after re-expose, got %d", len(ings.Items))
	}
	// fresh token: URL differs, expiry at least as far out as before (same-second
	// calls share the same second-granularity expiry timestamp)
	if url1 == url2 {
		t.Fatal("expected a fresh token on re-expose")
	}
	if exp2 < exp1 {
		t.Fatalf("expected refreshed expiry, got %d then %d", exp1, exp2)
	}
	if _, err := o.k8s.CoreV1().Services(sandboxNS).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Service should still exist: %v", err)
	}
}

func TestExposePort_TTLDefaultsToSessionTTL(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	// session with no explicit TTL → default 3600
	sess := mustCreateSession(t, o, "expose-ttl")
	_, exp := mustExpose(t, o, sess.Name, 3000, 0)
	if exp <= time.Now().Unix()+3600-30 || exp > time.Now().Unix()+3600+30 {
		t.Fatalf("expected default TTL ~3600s, got expiry %d", exp)
	}
}

func TestExposePort_NotActiveSession(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	ctx := context.Background()
	sess := mustCreateSession(t, o, "expose-inactive")
	// mark terminating
	sess.Status.Phase = "Terminating"
	o.updateSessionStatus(ctx, sess)

	_, _, err := o.ExposePort(ctx, sess.Name, 8080, 60)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for non-active session, got %v", err)
	}
}

func TestExposePort_MissingSession(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	_, _, err := o.ExposePort(context.Background(), "nope", 8080, 60)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestUnexposePort_RemovesRouteImmediately(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	ctx := context.Background()
	sess := mustCreateSession(t, o, "unexpose-1")
	mustExpose(t, o, sess.Name, 8080, 3600)

	name := previewResourceName(sess.Name, 8080)
	if err := o.UnexposePort(ctx, sess.Name, 8080); err != nil {
		t.Fatalf("unexpose: %v", err)
	}
	if _, err := o.k8s.CoreV1().Services(sandboxNS).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatal("expected preview Service deleted")
	}
	if _, err := o.k8s.NetworkingV1().Ingresses(sandboxNS).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatal("expected preview Ingress deleted")
	}
	got, err := o.getSession(ctx, sess.Name)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(got.Status.ExposedPorts) != 0 {
		t.Fatalf("expected exposedPorts emptied, got %+v", got.Status.ExposedPorts)
	}
	if !o.preview.IsRevoked(sess.Name, 8080) {
		t.Fatal("expected port marked revoked")
	}
}

func TestDestroySession_RevokesPreviewResources(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	ctx := context.Background()
	sess := mustCreateSession(t, o, "gc-preview")
	mustExpose(t, o, sess.Name, 8080, 3600)
	mustExpose(t, o, sess.Name, 9090, 3600)

	// simulate GC: destroy the expired session
	sess.Status.Phase = "Active"
	o.updateSessionStatus(ctx, sess)
	if err := o.DestroySession(ctx, sess.Name); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	svcs, _ := o.k8s.CoreV1().Services(sandboxNS).List(ctx, metav1.ListOptions{LabelSelector: previewLabel + "=" + previewLabelValue})
	if len(svcs.Items) != 0 {
		t.Fatalf("expected all preview Services revoked, got %d", len(svcs.Items))
	}
	ings, _ := o.k8s.NetworkingV1().Ingresses(sandboxNS).List(ctx, metav1.ListOptions{LabelSelector: previewLabel + "=" + previewLabelValue})
	if len(ings.Items) != 0 {
		t.Fatalf("expected all preview Ingresses revoked, got %d", len(ings.Items))
	}
	if !o.preview.IsRevoked(sess.Name, 8080) {
		t.Fatal("expected session-level revocation marker")
	}
}

func TestPreviewAuthorize_Valid(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	sess := mustCreateSession(t, o, "auth-ok")
	mustExpose(t, o, sess.Name, 8080, 3600)
	// re-fetch to get the current URL/token
	got, _ := o.getSession(context.Background(), sess.Name)
	tok := previewTokenFromURL(t, got.Status.ExposedPorts[0].URL)
	if _, _, err := o.preview.authorizePreview(tok); err != nil {
		t.Fatalf("expected authorize to pass: %v", err)
	}
}

func TestPreviewAuthorize_RevokedPort(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	sess := mustCreateSession(t, o, "auth-revoked")
	mustExpose(t, o, sess.Name, 8080, 3600)
	got, _ := o.getSession(context.Background(), sess.Name)
	tok := previewTokenFromURL(t, got.Status.ExposedPorts[0].URL)

	o.preview.RevokePort(sess.Name, 8080)
	if _, _, err := o.preview.authorizePreview(tok); err == nil {
		t.Fatal("expected authorize to fail after RevokePort")
	}
}

func TestPreviewAuthorize_PortUnpublished(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	sess := mustCreateSession(t, o, "auth-unpub")
	mustExpose(t, o, sess.Name, 8080, 3600)
	got, _ := o.getSession(context.Background(), sess.Name)
	tok := previewTokenFromURL(t, got.Status.ExposedPorts[0].URL)

	// clear the port from status (as unexpose does)
	got.Status.ExposedPorts = nil
	o.updateSessionStatus(context.Background(), got)

	if _, _, err := o.preview.authorizePreview(tok); err == nil {
		t.Fatal("expected authorize to fail for unpublished port")
	}
}

func TestPreviewAuthorize_SessionGone(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	sess := mustCreateSession(t, o, "auth-gone")
	mustExpose(t, o, sess.Name, 8080, 3600)
	got, _ := o.getSession(context.Background(), sess.Name)
	tok := previewTokenFromURL(t, got.Status.ExposedPorts[0].URL)

	// GC path destroys the session entirely
	if err := o.DestroySession(context.Background(), sess.Name); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, _, err := o.preview.authorizePreview(tok); err == nil {
		t.Fatal("expected authorize to fail after session destroyed")
	}
}

func TestPreviewAuthorize_ExpiredToken(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	sess := mustCreateSession(t, o, "auth-expired")
	// mint a token that is already expired
	expired, err := o.preview.mintToken(sess.Name, 8080, -5)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, _, err := o.preview.authorizePreview(expired); err == nil {
		t.Fatal("expected authorize to fail for expired token")
	}
}

func TestPreviewVerifyHandler_HTTP(t *testing.T) {
	o := newTestPreviewOrchestrator(t)
	sess := mustCreateSession(t, o, "verify-http")
	mustExpose(t, o, sess.Name, 8080, 3600)
	got, _ := o.getSession(context.Background(), sess.Name)
	tok := previewTokenFromURL(t, got.Status.ExposedPorts[0].URL)

	handler := http.HandlerFunc(o.preview.ServePreviewVerify)

	// valid token → 200
	req := httptest.NewRequest(http.MethodGet, "/preview/verify", nil)
	req.Header.Set("X-Original-URI", "/p/verify-http/8080/"+tok+"/")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid token, got %d", rr.Code)
	}

	// invalid token → 403
	req2 := httptest.NewRequest(http.MethodGet, "/preview/verify", nil)
	req2.Header.Set("X-Original-URI", "/p/verify-http/8080/not-a-real-token/")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for invalid token, got %d", rr2.Code)
	}

	// unexposed port → 403
	if err := o.UnexposePort(context.Background(), sess.Name, 8080); err != nil {
		t.Fatalf("unexpose: %v", err)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/preview/verify", nil)
	req3.Header.Set("X-Original-URI", "/p/verify-http/8080/"+tok+"/")
	rr3 := httptest.NewRecorder()
	handler.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unexposed port, got %d", rr3.Code)
	}
}

// previewTokenFromURL extracts the token segment from a preview URL.
func previewTokenFromURL(t *testing.T, url string) string {
	t.Helper()
	// https://host/p/<sid>/<port>/<token>/
	parts := strings.Split(strings.TrimSuffix(url, "/"), "/")
	if len(parts) < 2 {
		t.Fatalf("unparseable preview URL %q", url)
	}
	return parts[len(parts)-1]
}
