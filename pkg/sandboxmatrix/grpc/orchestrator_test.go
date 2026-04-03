package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

func newTestOrchestrator() *Orchestrator {
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "k8e.cattle.io", Version: "v1alpha1", Kind: "SandboxSession"},
		{Group: "k8e.cattle.io", Version: "v1alpha1", Kind: "SandboxMatrix"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "k8e.cattle.io", Version: "v1alpha1", Kind: "SandboxSessionList"},
		{Group: "k8e.cattle.io", Version: "v1alpha1", Kind: "SandboxMatrixList"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicyList"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
	}
	// explicit resource→listKind mapping to avoid fake client pluralisation bugs
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxsessions"}:  "SandboxSessionList",
		{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxmatrices"}:  "SandboxMatrixList",
		{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}:       "CiliumNetworkPolicyList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	k8s := kubefake.NewSimpleClientset()
	return NewOrchestrator(k8s, dyn)
}

func TestCreateSession_GeneratesID(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Name == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestCreateSession_DefaultRuntime(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "test-rt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Spec.RuntimeClass != "gvisor" {
		t.Fatalf("expected default runtime gvisor, got %s", sess.Spec.RuntimeClass)
	}
}

func TestRunSubAgent_MaxDepthEnforced(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8e.cattle.io/v1alpha1",
			"kind":       "SandboxSession",
			"metadata":   map[string]interface{}{"name": "parent-deep", "namespace": sandboxNS},
			"spec":       map[string]interface{}{"depth": int64(1), "runtimeClass": "gvisor"},
		},
	}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, parent, metav1.CreateOptions{})

	_, err := o.RunSubAgent(ctx, &pb.RunSubAgentRequest{ParentSessionId: "parent-deep"})
	if err == nil {
		t.Fatal("expected PermissionDenied error")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
	}
}

func TestDestroySession_NotFound(t *testing.T) {
	o := newTestOrchestrator()
	err := o.DestroySession(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestCreateSession_CustomSessionID(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "my-session"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Name != "my-session" {
		t.Fatalf("expected session ID my-session, got %s", sess.Name)
	}
}

func TestCreateSession_AllowedHosts(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId:    "hosts-test",
		AllowedHosts: []string{"example.com", "api.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sess.Spec.AllowedHosts) != 2 || sess.Spec.AllowedHosts[0] != "example.com" {
		t.Fatalf("unexpected allowed_hosts: %v", sess.Spec.AllowedHosts)
	}
}

func TestCreateSession_ExpiresAt_WithTTL(t *testing.T) {
	o := newTestOrchestrator()
	// seed a SandboxMatrix with sessionTTL=3600
	matrix := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8e.cattle.io/v1alpha1",
		"kind":       "SandboxMatrix",
		"metadata":   map[string]interface{}{"name": "default", "namespace": sandboxNS},
		"spec":       map[string]interface{}{"sessionTTL": int64(3600)},
	}}
	o.dynamic.Resource(matrixGVR).Namespace(sandboxNS).Create(context.Background(), matrix, metav1.CreateOptions{})

	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "ttl-test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Status.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set when sessionTTL > 0")
	}
}

func TestCreateSession_ExpiresAt_NoTTL(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "no-ttl"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Status.ExpiresAt != nil {
		t.Fatal("expected ExpiresAt to be nil when no TTL configured")
	}
}

func TestCreateSession_CreatesPVC(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "pvc-test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Status.WorkspacePVC == "" {
		t.Fatal("expected WorkspacePVC to be set")
	}
	// verify PVC exists in fake k8s
	pvc, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Get(context.Background(), sess.Status.WorkspacePVC, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC not found: %v", err)
	}
	if pvc.Labels[labelSessionID] != "pvc-test" {
		t.Fatalf("PVC missing session label, got %v", pvc.Labels)
	}
}

func TestDestroySession_DeletesPodAndPVC(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	sess, err := o.CreateSession(ctx, &pb.CreateSessionRequest{SessionId: "destroy-test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	podName := sess.Status.PodName
	pvcName := sess.Status.WorkspacePVC

	if err := o.DestroySession(ctx, "destroy-test"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// pod should be gone
	if podName != "" {
		_, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			t.Error("expected pod to be deleted")
		}
	}
	// PVC should be gone
	if pvcName != "" {
		_, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Get(ctx, pvcName, metav1.GetOptions{})
		if err == nil {
			t.Error("expected PVC to be deleted")
		}
	}
}

func TestDestroySession_DeletesCNP(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	_, err := o.CreateSession(ctx, &pb.CreateSessionRequest{SessionId: "cnp-test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// CNP should exist
	cnpName := "sandbox-session-cnp-test"
	_, err = o.dynamic.Resource(cnpGVR).Namespace(sandboxNS).Get(ctx, cnpName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("CNP not found after create: %v", err)
	}

	if err := o.DestroySession(ctx, "cnp-test"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	_, err = o.dynamic.Resource(cnpGVR).Namespace(sandboxNS).Get(ctx, cnpName, metav1.GetOptions{})
	if err == nil {
		t.Error("expected CNP to be deleted after destroy")
	}
}

func TestListActiveSessions_FiltersPhase(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	// create one active, one terminating
	for _, name := range []string{"active-1", "active-2"} {
		o.CreateSession(ctx, &pb.CreateSessionRequest{SessionId: name}) //nolint:errcheck
	}
	// manually insert a terminating session
	term := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8e.cattle.io/v1alpha1",
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "term-1", "namespace": sandboxNS},
		"spec":       map[string]interface{}{},
		"status":     map[string]interface{}{"phase": "Terminating"},
	}}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, term, metav1.CreateOptions{}) //nolint:errcheck

	sessions, err := o.ListActiveSessions(ctx, sandboxNS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(sessions))
	}
}

func TestRunSubAgent_Success(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	// create parent at depth 0
	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8e.cattle.io/v1alpha1",
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "parent-ok", "namespace": sandboxNS},
		"spec":       map[string]interface{}{"depth": int64(0), "runtimeClass": "gvisor", "allowedHosts": []interface{}{"pypi.org"}},
	}}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, parent, metav1.CreateOptions{}) //nolint:errcheck

	resp, err := o.RunSubAgent(ctx, &pb.RunSubAgentRequest{ParentSessionId: "parent-ok", AgentType: "coding"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.SessionId == "" {
		t.Fatal("expected non-empty child session ID")
	}
	// verify child depth = 1
	child, err := o.getSession(ctx, resp.SessionId)
	if err != nil {
		t.Fatalf("child session not found: %v", err)
	}
	if child.Spec.Depth != 1 {
		t.Fatalf("expected child depth 1, got %d", child.Spec.Depth)
	}
}

func TestConfirmAction_RegisterAndApprove(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	// register
	resp, err := o.ConfirmAction(ctx, &pb.ConfirmActionRequest{
		SessionId: "sess-1",
		Action:    "delete /workspace/report.pdf",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.ApprovalId == "" {
		t.Fatal("expected approval_id")
	}
	if resp.Approved {
		t.Fatal("should not be approved yet")
	}

	// approve externally
	go o.Approve(resp.ApprovalId, true) //nolint:errcheck

	// poll
	poll, err := o.ConfirmAction(ctx, &pb.ConfirmActionRequest{
		SessionId:  "sess-1",
		ApprovalId: resp.ApprovalId,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !poll.Approved {
		t.Fatal("expected approved=true")
	}
}

func TestGCExpiredSessions_DestroysExpired(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	// create a session then manually set ExpiresAt to the past
	sess, err := o.CreateSession(ctx, &pb.CreateSessionRequest{SessionId: "gc-expired"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// backdate ExpiresAt
	u, _ := o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sess.Name, metav1.GetOptions{})
	status := u.Object["status"].(map[string]interface{})
	status["expiresAt"] = "2000-01-01T00:00:00Z"
	status["phase"] = "Active"
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).UpdateStatus(ctx, u, metav1.UpdateOptions{}) //nolint:errcheck

	sessions, _ := o.ListActiveSessions(ctx, sandboxNS)
	destroyed := 0
	for _, s := range sessions {
		if s.Status.ExpiresAt != nil && s.Status.ExpiresAt.Time.Before(time.Now()) {
			o.DestroySession(ctx, s.Name) //nolint:errcheck
			destroyed++
		}
	}
	if destroyed != 1 {
		t.Fatalf("expected 1 session destroyed, got %d", destroyed)
	}
	// session should be gone
	_, err = o.getSession(ctx, "gc-expired")
	if err == nil {
		t.Fatal("expected session to be deleted")
	}
}

func TestGCExpiredSessions_KeepsNonExpired(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	sess, err := o.CreateSession(ctx, &pb.CreateSessionRequest{SessionId: "gc-keep"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// set ExpiresAt to the future
	u, _ := o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sess.Name, metav1.GetOptions{})
	status := u.Object["status"].(map[string]interface{})
	status["expiresAt"] = "2099-01-01T00:00:00Z"
	status["phase"] = "Active"
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).UpdateStatus(ctx, u, metav1.UpdateOptions{}) //nolint:errcheck

	sessions, _ := o.ListActiveSessions(ctx, sandboxNS)
	for _, s := range sessions {
		if s.Status.ExpiresAt != nil && s.Status.ExpiresAt.Time.Before(time.Now()) {
			t.Fatal("should not destroy future-expiry session")
		}
	}
	// session should still exist
	if _, err := o.getSession(ctx, "gc-keep"); err != nil {
		t.Fatalf("session should still exist: %v", err)
	}
}
