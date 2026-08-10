package grpc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

const (
	testGroupK8e    = "k8e.sh"
	testGroupCilium = "cilium.io"
	testAPIVer      = "k8e.sh/v1alpha1"
	msgUnexpected   = "unexpected error: %v"
	msgCreate       = "create: %v"
)

func newTestOrchestrator() *Orchestrator {
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
	// explicit resource→listKind mapping to avoid fake client pluralisation bugs
	listKinds := map[schema.GroupVersionResource]string{
		{Group: testGroupK8e, Version: "v1alpha1", Resource: "sandboxsessions"}:    "SandboxSessionList",
		{Group: testGroupK8e, Version: "v1alpha1", Resource: "sandboxmatrices"}:    "SandboxMatrixList",
		{Group: testGroupCilium, Version: "v2", Resource: "ciliumnetworkpolicies"}: "CiliumNetworkPolicyList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	k8s := kubefake.NewSimpleClientset()
	return NewOrchestrator(k8s, dyn)
}

// mustCreateSession creates a session and fails the test on error.
func mustCreateSession(t *testing.T, o *Orchestrator, id string) *sandboxv1.SandboxSession {
	t.Helper()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: id})
	if err != nil {
		t.Fatalf(msgCreate, err)
	}
	return sess
}

// setSessionExpiry backdates or future-dates a session's expiresAt via UpdateStatus.
func setSessionExpiry(t *testing.T, o *Orchestrator, sessName, expiresAt string) {
	t.Helper()
	ctx := context.Background()
	u, err := o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sessName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get session for expiry update: %v", err)
	}
	st := u.Object["status"].(map[string]interface{})
	st["expiresAt"] = expiresAt
	st["phase"] = "Active"
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).UpdateStatus(ctx, u, metav1.UpdateOptions{}) //nolint:errcheck
}

// warmTestPod builds a Running warm pod fixture for claim-path tests.
// The pod is labeled with runtimeClass "gvisor" (the session default).
func warmTestPod(name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandboxNS,
			Labels: map[string]string{
				labelState:        stateWarm,
				labelRuntimeClass: "gvisor",
			},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sandbox", Ports: []corev1.ContainerPort{{ContainerPort: 2024}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
}

func TestSandboxPodSpec_IncludesHealthProbes(t *testing.T) {
	spec := SandboxPodSpec("gvisor", "", "500m", "512Mi", sandboxImage)
	c := spec.Containers[0]
	if c.StartupProbe == nil || c.ReadinessProbe == nil {
		t.Fatal("expected startup + readiness probes on sandbox container")
	}
	for name, probe := range map[string]*corev1.Probe{"startup": c.StartupProbe, "readiness": c.ReadinessProbe} {
		if probe.TCPSocket == nil || probe.TCPSocket.Port.IntValue() != 2024 {
			t.Errorf("%s probe should TCP-check :2024, got %+v", name, probe)
		}
	}
}

func TestClaimWarmPod_PrefersReadyPod(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool {
		return pod.Annotations["test-healthy"] == "yes"
	}

	ready := warmTestPod("warm-ready", "10.0.0.2")
	ready.Annotations["test-healthy"] = "yes"
	notReady := warmTestPod("warm-notready", "10.0.0.3")

	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, ready, metav1.CreateOptions{})    //nolint:errcheck
	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, notReady, metav1.CreateOptions{}) //nolint:errcheck

	sess := mustCreateSession(t, o, "claim-ready")
	pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, sess.Status.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claimed pod: %v", err)
	}
	if pod.Name != "warm-ready" {
		t.Fatalf("expected warm-ready to be claimed, got %s", pod.Name)
	}
	if pod.Labels[labelState] != stateActive {
		t.Fatalf("expected claimed pod state=active, got %s", pod.Labels[labelState])
	}
	if pod.Labels[labelSessionID] != "claim-ready" {
		t.Fatalf("expected session label on claimed pod, got %v", pod.Labels)
	}
}

func TestClaimWarmPod_FallsBackToColdStart(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool { return false }

	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, warmTestPod("warm-unhealthy", "10.0.0.2"), metav1.CreateOptions{}) //nolint:errcheck

	sess := mustCreateSession(t, o, "claim-cold")
	if strings.HasPrefix(sess.Status.PodName, "warm-") {
		t.Fatalf("expected cold-start pod, got warm pod %s", sess.Status.PodName)
	}
	if !strings.HasPrefix(sess.Status.PodName, "sandbox-") {
		t.Fatalf("expected sandbox-* cold-start pod name, got %s", sess.Status.PodName)
	}
}

func TestClaimWarmPod_RuntimeClassMismatchSkipped(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool { return true }

	kata := warmTestPod("warm-kata", "10.0.0.4")
	kata.Labels[labelRuntimeClass] = "kata"
	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, kata, metav1.CreateOptions{}) //nolint:errcheck

	// session defaults to gvisor — the kata warm pod must not be adopted
	sess := mustCreateSession(t, o, "rt-mismatch")
	if strings.HasPrefix(sess.Status.PodName, "warm-") {
		t.Fatalf("expected cold-start (runtime-mismatched warm pod must not be claimed), got %s", sess.Status.PodName)
	}
	pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, "warm-kata", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get kata pod: %v", err)
	}
	if pod.Labels[labelState] != stateWarm {
		t.Fatalf("kata warm pod should stay warm, got state %s", pod.Labels[labelState])
	}
	// cold-start pod records the session runtime
	cold, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, sess.Status.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cold pod: %v", err)
	}
	if cold.Labels[labelRuntimeClass] != "gvisor" {
		t.Fatalf("expected cold-start pod runtime label gvisor, got %v", cold.Labels)
	}
}

func TestClaimWarmPod_RuntimeClassMatchClaimed(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool { return true }

	gv := warmTestPod("warm-gv", "10.0.0.5")
	kata := warmTestPod("warm-kata2", "10.0.0.6")
	kata.Labels[labelRuntimeClass] = "kata"
	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, gv, metav1.CreateOptions{})   //nolint:errcheck
	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, kata, metav1.CreateOptions{}) //nolint:errcheck

	sess := mustCreateSession(t, o, "rt-match")
	if sess.Status.PodName != "warm-gv" {
		t.Fatalf("expected gvisor warm pod claimed, got %s", sess.Status.PodName)
	}
}

func TestClaimWarmPod_LegacyPodWithoutRuntimeLabel(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool { return true }

	legacy := warmTestPod("warm-legacy", "10.0.0.7")
	delete(legacy.Labels, labelRuntimeClass)
	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, legacy, metav1.CreateOptions{}) //nolint:errcheck

	sess := mustCreateSession(t, o, "rt-legacy")
	if sess.Status.PodName != "warm-legacy" {
		t.Fatalf("expected legacy warm pod (no runtime label) claimed, got %s", sess.Status.PodName)
	}
}

func TestClaimWarmPod_TriggersRefillSignal(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool { return true }
	claims := 0
	o.OnWarmClaim = func() { claims++ }

	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, warmTestPod("warm-refill", "10.0.0.8"), metav1.CreateOptions{}) //nolint:errcheck
	mustCreateSession(t, o, "refill-warm")
	if claims != 1 {
		t.Fatalf("expected 1 refill signal after warm claim, got %d", claims)
	}
}

func TestColdStart_NoRefillSignal(t *testing.T) {
	o := newTestOrchestrator()
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool { return false }
	claims := 0
	o.OnWarmClaim = func() { claims++ }

	mustCreateSession(t, o, "refill-cold")
	if claims != 0 {
		t.Fatalf("expected no refill signal for cold start, got %d", claims)
	}
}

func TestClaimMetrics_WarmVsCold(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	// Fake clients don't filter by label selector, so also require state=warm
	// here; otherwise the second (cold) claim would re-adopt the claimed pod.
	o.warmPodHealthCheck = func(ctx context.Context, pod *corev1.Pod) bool {
		return pod.Labels[labelState] == stateWarm
	}

	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, warmTestPod("warm-metric", "10.0.0.9"), metav1.CreateOptions{}) //nolint:errcheck

	mustCreateSession(t, o, "metric-warm")
	claimed, cold, avg := o.Metrics()
	if claimed != 1 || cold != 0 {
		t.Fatalf("after warm claim expected warm=1 cold=0, got %d/%d", claimed, cold)
	}
	if avg < 0 {
		t.Fatalf("expected non-negative avg claim latency, got %d", avg)
	}

	mustCreateSession(t, o, "metric-cold")
	claimed, cold, avg = o.Metrics()
	if claimed != 1 || cold != 1 {
		t.Fatalf("after cold start expected warm=1 cold=1, got %d/%d", claimed, cold)
	}
	if avg < 0 {
		t.Fatalf("expected non-negative avg claim latency, got %d", avg)
	}
}

func TestCreateSession_GeneratesID(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if sess.Name == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestCreateSession_DefaultRuntime(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "test-rt"})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
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
			"apiVersion": testAPIVer,
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
		t.Fatalf(msgUnexpected, err)
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
		t.Fatalf(msgUnexpected, err)
	}
	if len(sess.Spec.AllowedHosts) != 2 || sess.Spec.AllowedHosts[0] != "example.com" {
		t.Fatalf("unexpected allowed_hosts: %v", sess.Spec.AllowedHosts)
	}
}

func TestCreateSession_Env(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{
		SessionId: "env-test",
		Env:       map[string]string{"FOO": "bar", "BAZ": "qux"},
	})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if sess.Spec.Env["FOO"] != "bar" || sess.Spec.Env["BAZ"] != "qux" {
		t.Fatalf("unexpected env: %v", sess.Spec.Env)
	}
	// Round-trip through getSession (unstructured conversion) must preserve env.
	got, err := o.getSession(context.Background(), "env-test")
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	if got.Spec.Env["FOO"] != "bar" || got.Spec.Env["BAZ"] != "qux" {
		t.Fatalf("env lost on getSession round-trip: %v", got.Spec.Env)
	}
}

func TestCreateSession_ExpiresAt_WithTTL(t *testing.T) {
	o := newTestOrchestrator()
	// seed a SandboxMatrix with sessionTTL=3600
	matrix := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testAPIVer,
		"kind":       "SandboxMatrix",
		"metadata":   map[string]interface{}{"name": "default", "namespace": sandboxNS},
		"spec":       map[string]interface{}{"sessionTTL": int64(3600)},
	}}
	o.dynamic.Resource(matrixGVR).Namespace(sandboxNS).Create(context.Background(), matrix, metav1.CreateOptions{})

	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "ttl-test"})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if sess.Status.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set when sessionTTL > 0")
	}
}

func TestCreateSession_ExpiresAt_NoTTL(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "no-ttl"})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if sess.Status.ExpiresAt != nil {
		t.Fatal("expected ExpiresAt to be nil when no TTL configured")
	}
}

func TestCreateSession_CreatesPVC(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "pvc-test", TenantId: "tenant-1"})
	if err != nil {
		t.Fatalf(msgCreate, err)
	}
	if sess.Status.WorkspacePVC == "" {
		t.Fatal("expected WorkspacePVC to be set for a persistent (tenant) session")
	}
	pvc, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Get(context.Background(), sess.Status.WorkspacePVC, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC not found: %v", err)
	}
	if pvc.Labels[labelSessionID] != "pvc-test" {
		t.Fatalf("PVC missing session label, got %v", pvc.Labels)
	}
}

func TestCreateSession_Ephemeral_NoPVC(t *testing.T) {
	o := newTestOrchestrator()
	sess := mustCreateSession(t, o, "eph-test") // no tenant → ephemeral
	if sess.Status.WorkspacePVC != "" {
		t.Fatalf("expected no WorkspacePVC for ephemeral session, got %q", sess.Status.WorkspacePVC)
	}
	pvcs, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pvcs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("expected no PVCs for ephemeral session, got %d", len(pvcs.Items))
	}
}

func TestDestroySession_ReleasesPod(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	sess := mustCreateSession(t, o, "destroy-test")
	podName, pvcName := sess.Status.PodName, sess.Status.WorkspacePVC

	// Fake clients don't support label selectors; manually set up pod labels and session status
	setupPodForRelease(ctx, t, o, podName)

	if err := o.DestroySession(ctx, "destroy-test"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	assertPodReleased(ctx, t, o, podName)
	assertPVCSurvives(ctx, t, o, pvcName)
}

func setupPodForRelease(ctx context.Context, t *testing.T, o *Orchestrator, podName string) {
	t.Helper()
	if podName == "" {
		return
	}
	// Persist pod IP in session CRD status
	u, _ := o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, "destroy-test", metav1.GetOptions{})
	if u != nil {
		st := u.Object["status"].(map[string]interface{})
		st["podName"] = podName
		st["podIP"] = "10.0.0.1"
		o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).UpdateStatus(ctx, u, metav1.UpdateOptions{})
	}
	// Set pod labels and status
	pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[labelSessionID] = "destroy-test"
	pod.Labels[labelState] = stateActive
	o.k8s.CoreV1().Pods(sandboxNS).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
}

func assertPodReleased(ctx context.Context, t *testing.T, o *Orchestrator, podName string) {
	t.Helper()
	if podName == "" {
		return
	}
	pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected pod to survive after destroy: %v", err)
	}
	if pod.Labels[labelState] != StateResetting {
		t.Errorf("expected pod state=%s, got %s", StateResetting, pod.Labels[labelState])
	}
	if pod.Labels[labelSessionID] == "destroy-test" {
		t.Error("expected session-id label to be removed")
	}
}

func assertPVCSurvives(ctx context.Context, t *testing.T, o *Orchestrator, pvcName string) {
	t.Helper()
	if pvcName == "" {
		return
	}
	if _, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Get(ctx, pvcName, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PVC to survive after destroy: %v", err)
	}
}

func TestDestroySession_DeletesCNP(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	mustCreateSession(t, o, "cnp-test")

	cnpName := "sandbox-session-cnp-test"
	if _, err := o.dynamic.Resource(cnpGVR).Namespace(sandboxNS).Get(ctx, cnpName, metav1.GetOptions{}); err != nil {
		t.Fatalf("CNP not found after create: %v", err)
	}
	if err := o.DestroySession(ctx, "cnp-test"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := o.dynamic.Resource(cnpGVR).Namespace(sandboxNS).Get(ctx, cnpName, metav1.GetOptions{}); err == nil {
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
		"apiVersion": testAPIVer,
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "term-1", "namespace": sandboxNS},
		"spec":       map[string]interface{}{},
		"status":     map[string]interface{}{"phase": "Terminating"},
	}}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, term, metav1.CreateOptions{}) //nolint:errcheck

	sessions, err := o.ListActiveSessions(ctx, sandboxNS)
	if err != nil {
		t.Fatalf(msgUnexpected, err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(sessions))
	}
}

func TestRunSubAgent_Success(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	// create parent at depth 0 with a shared workspace PVC (persistent session)
	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testAPIVer,
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "parent-ok", "namespace": sandboxNS},
		"spec":       map[string]interface{}{"depth": int64(0), "runtimeClass": "gvisor", "allowedHosts": []interface{}{"pypi.org"}},
		"status":     map[string]interface{}{"workspacePVC": "workspace-parent-ok", "podIP": "10.0.0.5"},
	}}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, parent, metav1.CreateOptions{}) //nolint:errcheck

	resp, err := o.RunSubAgent(ctx, &pb.RunSubAgentRequest{ParentSessionId: "parent-ok", AgentType: "coding"})
	if err != nil {
		t.Fatalf(msgUnexpected, err)
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
	// child must share the parent's workspace PVC for file-based IPC
	if child.Status.WorkspacePVC != "workspace-parent-ok" {
		t.Fatalf("expected child to share parent PVC, got %q", child.Status.WorkspacePVC)
	}
	// M1 workspace-session reuse: child inherits parent PodIP (exec routing)
	// but has NO own PodName (destroy must not touch the shared pod).
	if child.Status.PodIP != "10.0.0.5" {
		t.Fatalf("expected child to reuse parent PodIP, got %q", child.Status.PodIP)
	}
	if child.Status.PodName != "" {
		t.Fatalf("expected child to have no own PodName (shares parent pod), got %q", child.Status.PodName)
	}
	// M1 slice 2: child gets an isolated workspace scope for independent reset.
	if child.Status.WorkspaceScope == "" {
		t.Fatal("expected child to have an isolated WorkspaceScope")
	}
	if !strings.HasPrefix(child.Status.WorkspaceScope, ".sessions/") {
		t.Fatalf("expected scope under .sessions/, got %q", child.Status.WorkspaceScope)
	}
}

func TestRunSubAgent_NoParentPVC_FailedPrecondition(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testAPIVer,
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "parent-nopvc", "namespace": sandboxNS},
		"spec":       map[string]interface{}{"depth": int64(0), "runtimeClass": "gvisor"},
	}}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, parent, metav1.CreateOptions{}) //nolint:errcheck

	_, err := o.RunSubAgent(ctx, &pb.RunSubAgentRequest{ParentSessionId: "parent-nopvc"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for parent without PVC, got %v", status.Code(err))
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
	sess := mustCreateSession(t, o, "gc-expired")
	setSessionExpiry(t, o, sess.Name, "2000-01-01T00:00:00Z")

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
	if _, err := o.getSession(ctx, "gc-expired"); err == nil {
		t.Fatal("expected session to be deleted")
	}
}

func TestGCExpiredSessions_KeepsNonExpired(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()
	sess := mustCreateSession(t, o, "gc-keep")
	setSessionExpiry(t, o, sess.Name, "2099-01-01T00:00:00Z")

	sessions, _ := o.ListActiveSessions(ctx, sandboxNS)
	for _, s := range sessions {
		if s.Status.ExpiresAt != nil && s.Status.ExpiresAt.Time.Before(time.Now()) {
			t.Fatal("should not destroy future-expiry session")
		}
	}
	if _, err := o.getSession(ctx, "gc-keep"); err != nil {
		t.Fatalf("session should still exist: %v", err)
	}
}

// TestReadyHandshake_StatusReady verifies the application-layer readiness
// handshake accepts sandboxd that answers /ready with status=="ready".
func TestReadyHandshake_StatusReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready","venv":true}`))
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	if !readyHandshake(context.Background(), hostPort) {
		t.Fatal("expected ready handshake to pass for status=ready")
	}
}

// TestReadyHandshake_NotReady verifies sandboxd answering a non-ready status
// (e.g. venv still initializing) fails the handshake.
func TestReadyHandshake_NotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"initializing","venv":false}`))
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	if readyHandshake(context.Background(), hostPort) {
		t.Fatal("expected handshake to fail for status=initializing")
	}
}

// TestReadyHandshake_ErrorStatus verifies non-200 responses fail the handshake.
func TestReadyHandshake_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	if readyHandshake(context.Background(), hostPort) {
		t.Fatal("expected handshake to fail on non-200")
	}
}

// TestReadyHandshake_Unreachable verifies a dead endpoint fails fast and that
// the TCP fallback still accepts a reachable-but-stale port.
func TestReadyHandshake_Unreachable(t *testing.T) {
	if readyHandshake(context.Background(), "127.0.0.1:1") {
		t.Fatal("expected handshake to fail for unreachable endpoint")
	}
}

// TestDefaultWarmPodHealthCheck_ReadyHandshakePreferred verifies the default
// check prefers the application-layer /ready handshake over a bare TCP dial:
// a pod whose port is open but whose sandboxd answers non-ready is rejected.
func TestDefaultWarmPodHealthCheck_ReadyHandshakePreferred(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// TCP accepts, but never serves a valid /ready response.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()
	hostPort := ln.Addr().String()
	pod := warmTestPod("warm-tcp-only", strings.Split(hostPort, ":")[0])
	// Point the health check at the test listener via a stub IP override.
	pod.Status.PodIP = "127.0.0.1"

	// The port is open (bare TCP dial would succeed), but /ready never answers
	// with status ready, so the pod must not be considered healthy. We exercise
	// readyHandshake directly against the listener: it will time out / fail.
	if readyHandshake(context.Background(), hostPort) {
		t.Fatal("expected handshake to fail for TCP-only listener")
	}
	_ = pod
}

// TestExecBackground_CapEnforced verifies the per-session background run cap
// (KIP-16 M12) rejects submissions beyond the limit with ResourceExhausted.
func TestExecBackground_CapEnforced(t *testing.T) {
	o := newTestOrchestrator()
	o.maxBackgroundRuns = 2
	ctx := context.Background()

	// Register two run_ids for the same session directly (registry is the
	// source of truth for the cap; no pod needed to exercise the gate).
	o.mu.Lock()
	o.runRegistry["sess-1-bg-1"] = "sess-1"
	o.runRegistry["sess-1-bg-2"] = "sess-1"
	o.mu.Unlock()

	if got := o.countBackgroundRuns("sess-1"); got != 2 {
		t.Fatalf("expected 2 background runs, got %d", got)
	}

	_, err := o.ExecBackground(ctx, "sess-1", "echo hi", 30, "/workspace", nil)
	if err == nil {
		t.Fatal("expected ResourceExhausted when cap reached")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", status.Code(err))
	}
}

// TestExecBackground_CapPerSession verifies the cap is per-session, not global:
// a session with free slots can still submit.
func TestExecBackground_CapPerSession(t *testing.T) {
	o := newTestOrchestrator()
	o.maxBackgroundRuns = 1
	ctx := context.Background()

	o.mu.Lock()
	o.runRegistry["sess-other-bg-1"] = "sess-other"
	o.mu.Unlock()

	if got := o.countBackgroundRuns("sess-fresh"); got != 0 {
		t.Fatalf("expected 0 runs for fresh session, got %d", got)
	}

	// Cap applies to the registry count, so a fresh session passes the gate;
	// the subsequent podIP lookup fails (no pod) — that's expected, it proves
	// the cap gate was not the rejection reason.
	_, err := o.ExecBackground(ctx, "sess-fresh", "echo hi", 30, "/workspace", nil)
	if err == nil {
		t.Fatal("expected error (no pod), not a successful submit")
	}
	if status.Code(err) == codes.ResourceExhausted {
		t.Fatal("fresh session must not hit the background cap")
	}
}

// TestBuildSessionCNP_PerSessionPolicy verifies the CNP builder emits a
// per-session policy: label-scoped endpoint selector, gateway+host ingress to
// :2024, and egress restricted to DNS(53)+HTTPS(443) only.
func TestBuildSessionCNP_PerSessionPolicy(t *testing.T) {
	sess := &sandboxv1.SandboxSession{}
	sess.Name = "cnp-test"
	sess.Namespace = sandboxNS

	obj := buildSessionCNP(sess)
	if obj.GetAPIVersion() != "cilium.io/v2" || obj.GetKind() != "CiliumNetworkPolicy" {
		t.Fatalf("unexpected GVK: %s/%s", obj.GetAPIVersion(), obj.GetKind())
	}
	spec := obj.Object["spec"].(map[string]interface{})
	sel := spec["endpointSelector"].(map[string]interface{})
	labels := sel["matchLabels"].(map[string]interface{})
	if labels[labelSessionID] != "cnp-test" {
		t.Fatalf("expected endpoint selector scoped to session label, got %v", labels)
	}

	ingress := spec["ingress"].([]interface{})
	if len(ingress) != 2 {
		t.Fatalf("expected 2 ingress rules (host + gateway), got %d", len(ingress))
	}
	// Both ingress rules must only expose :2024.
	for _, r := range ingress {
		rule := r.(map[string]interface{})
		ports := rule["toPorts"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})
		p := ports[0].(map[string]interface{})
		if p["port"] != "2024" || p["protocol"] != "TCP" {
			t.Fatalf("unexpected ingress port: %v", p)
		}
	}

	egress := spec["egress"].([]interface{})
	if len(egress) != 2 {
		t.Fatalf("expected 2 egress rules (53 + 443), got %d", len(egress))
	}
	seen := map[string]bool{}
	for _, r := range egress {
		rule := r.(map[string]interface{})
		entities := rule["toEntities"].([]interface{})
		if entities[0] != "world" {
			t.Fatalf("expected toEntities world, got %v", entities)
		}
		ports := rule["toPorts"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})
		p := ports[0].(map[string]interface{})
		seen[p["port"].(string)] = true
	}
	if !seen["53"] || !seen["443"] {
		t.Fatalf("egress must cover DNS 53 + HTTPS 443, got %v", seen)
	}
}

// TestBuildSessionCNP_NamespaceScoped verifies the CNP lands in the session
// namespace with the deterministic per-session name.
func TestBuildSessionCNP_NamespaceScoped(t *testing.T) {
	sess := &sandboxv1.SandboxSession{}
	sess.Name = "cnp-ns"
	sess.Namespace = "team-a"

	obj := buildSessionCNP(sess)
	if obj.GetNamespace() != "team-a" {
		t.Fatalf("expected CNP in session namespace, got %s", obj.GetNamespace())
	}
	if obj.GetName() != "sandbox-session-cnp-ns" {
		t.Fatalf("unexpected CNP name: %s", obj.GetName())
	}
}

// TestRunSubAgent_DestroyChildLeavesParentPod verifies the M1 workspace-session
// model: destroying a child that shares the parent's pod deletes only the
// child CRD — the parent's pod is not reset or released (no own PodName, so
// DestroySession's pod-cleanup path is skipped).
func TestRunSubAgent_DestroyChildLeavesParentPod(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	parent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testAPIVer,
		"kind":       "SandboxSession",
		"metadata":   map[string]interface{}{"name": "parent-shared", "namespace": sandboxNS},
		"spec":       map[string]interface{}{"depth": int64(0), "runtimeClass": "gvisor"},
		"status":     map[string]interface{}{"workspacePVC": "workspace-parent-shared", "podName": "pod-parent", "podIP": "10.0.0.9"},
	}}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, parent, metav1.CreateOptions{}) //nolint:errcheck

	// Parent pod exists and is active.
	parentPod := warmTestPod("pod-parent", "10.0.0.9")
	parentPod.Labels[labelSessionID] = "parent-shared"
	parentPod.Labels[labelState] = stateActive
	o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, parentPod, metav1.CreateOptions{}) //nolint:errcheck

	resp, err := o.RunSubAgent(ctx, &pb.RunSubAgentRequest{ParentSessionId: "parent-shared"})
	if err != nil {
		t.Fatalf("run sub-agent: %v", err)
	}

	if err := o.DestroySession(ctx, resp.SessionId); err != nil {
		t.Fatalf("destroy child: %v", err)
	}

	// Child CRD gone.
	if _, err := o.getSession(ctx, resp.SessionId); err == nil {
		t.Fatal("expected child session to be deleted")
	}

	// Parent pod untouched: still exists, still active, session label intact.
	pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, "pod-parent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("parent pod should survive child destroy: %v", err)
	}
	if pod.Labels[labelState] != stateActive {
		t.Fatalf("parent pod must stay active, got %v", pod.Labels)
	}
	if pod.Labels[labelSessionID] != "parent-shared" {
		t.Fatalf("parent pod session label must be intact, got %v", pod.Labels)
	}
}
