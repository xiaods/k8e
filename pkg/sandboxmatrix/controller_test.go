package sandboxmatrix

import (
	"context"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/daemons/config"
	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// testNS is the namespace the fixtures and the reconciler both target.
const testNS = "sandbox-matrix"

func defaultCfg() config.SandboxConfig {
	return config.SandboxConfig{
		DefaultRuntime: "gvisor",
		DefaultImage:   "ghcr.io/xiaods/k8e-sandbox:latest",
		DefaultCPU:     "500m",
		DefaultMemory:  "512Mi",
		GRPCPort:       50051,
		Namespace:      testNS,
	}
}

func fakeSandboxClients() (dynamic.Interface, kubernetes.Interface) {
	scheme := runtime.NewScheme()
	gvk := func(kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: kind}
	}
	scheme.AddKnownTypeWithName(gvk("SandboxWarmPool"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk("SandboxWarmPoolList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(gvk("SandboxMatrix"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk("SandboxMatrixList"), &unstructured.UnstructuredList{})
	listKinds := map[schema.GroupVersionResource]string{
		warmPoolGVR:    "SandboxWarmPoolList",
		localMatrixGVR: "SandboxMatrixList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds), kubefake.NewSimpleClientset()
}

// Pod fixtures. Every warm/active pod a test needs is one call away, so the
// tests stay about the behaviour under test instead of about building Pods.

var (
	// condReady: sandboxd is serving on :2024.
	condReady = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	// condScheduled: the scheduler bound the pod; it is pulling its image.
	condScheduled = corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}
	// condUnschedulable: the scheduler gave up on placing the pod.
	condUnschedulable = corev1.PodCondition{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: "0/1 nodes are available: 1 Insufficient memory.",
	}
)

// sandboxTestPod builds a pod in the sandbox namespace. An empty state leaves
// it unlabelled. The creation timestamp defaults to now, so a pod that is
// already old has to say so through aged().
func sandboxTestPod(name, state string, phase corev1.PodPhase, conditions ...corev1.PodCondition) *corev1.Pod {
	meta := metav1.ObjectMeta{Name: name, Namespace: testNS, CreationTimestamp: metav1.Now()}
	if state != "" {
		meta.Labels = map[string]string{sandboxgrpc.LabelState: state}
	}
	return &corev1.Pod{
		ObjectMeta: meta,
		Status:     corev1.PodStatus{Phase: phase, Conditions: conditions},
	}
}

// warmTestPod is a sandbox pod a session can claim.
func warmTestPod(name string, phase corev1.PodPhase, conditions ...corev1.PodCondition) *corev1.Pod {
	return sandboxTestPod(name, sandboxgrpc.StateWarm, phase, conditions...)
}

// aged backdates a pod's creation timestamp.
func aged(pod *corev1.Pod, d time.Duration) *corev1.Pod {
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-d))
	return pod
}

// idleSince stamps a pod as released back to the pool d ago.
func idleSince(pod *corev1.Pod, d time.Duration) *corev1.Pod {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[podReleasedAtAnnotation] = time.Now().Add(-d).UTC().Format(time.RFC3339)
	return pod
}

// seedPods creates the pods in the fake cluster.
func seedPods(ctx context.Context, t *testing.T, k8s kubernetes.Interface, pods ...*corev1.Pod) {
	t.Helper()
	for _, p := range pods {
		if _, err := k8s.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod %s: %v", p.Name, err)
		}
	}
}

// assertGone fails when a pod that should have been deleted is still there.
func assertGone(t *testing.T, k8s kubernetes.Interface, name, why string) {
	t.Helper()
	if _, err := k8s.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
		t.Errorf("expected %s to be deleted: %s", name, why)
	}
}

// assertPresent fails when a pod that should have survived is gone.
func assertPresent(t *testing.T, k8s kubernetes.Interface, name string) {
	t.Helper()
	if _, err := k8s.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Errorf("expected %s to survive: %v", name, err)
	}
}

func TestRecycleUnhealthyWarmPods(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()

	// Failed warm pod (sandboxd exited; RestartPolicy Never) — must be recycled.
	failed := warmTestPod("warm-failed", corev1.PodFailed)
	// Running warm pod stuck not-ready past the boot budget — must be recycled.
	stale := aged(warmTestPod("warm-stale", corev1.PodRunning), 10*time.Minute)
	// Fresh not-ready warm pod — still within boot budget, keep.
	fresh := warmTestPod("warm-fresh", corev1.PodRunning)
	// Healthy warm pod — keep.
	healthy := warmTestPod("warm-healthy", corev1.PodRunning, condReady)

	seedPods(ctx, t, k8s, failed, stale, fresh, healthy)

	recycleUnhealthyWarmPods(ctx, k8s, testNS)

	assertGone(t, k8s, "warm-failed", "sandboxd exited")
	assertGone(t, k8s, "warm-stale", "not ready past the boot budget")
	assertPresent(t, k8s, "warm-fresh")
	assertPresent(t, k8s, "warm-healthy")
}

func TestNewWarmPod_RuntimeClassLabel(t *testing.T) {
	pod := newWarmPod(defaultCfg(), "kata", 0)
	if pod.Labels[sandboxgrpc.LabelRuntimeClass] != "kata" {
		t.Fatalf("expected runtime-class label kata, got %v", pod.Labels)
	}
}

func TestNewWarmPod_IdleTTLAnnotation(t *testing.T) {
	pod := newWarmPod(defaultCfg(), "gvisor", 300)
	if pod.Annotations[podIdleTTLAnnotation] != "300" {
		t.Fatalf("expected idle-ttl annotation 300, got %v", pod.Annotations)
	}
	plain := newWarmPod(defaultCfg(), "gvisor", 0)
	if _, present := plain.Annotations[podIdleTTLAnnotation]; present {
		t.Fatal("expected no idle-ttl annotation when TTL unset")
	}
}

func TestAdaptiveTarget(t *testing.T) {
	cases := []struct {
		name                        string
		size, min, max, boost, want int64
	}{
		{"static no max", 2, 0, 0, 5, 2},
		{"static max equals size", 2, 0, 2, 5, 2},
		{"grow on burst", 2, 0, 8, 5, 5},
		{"bounded by max", 2, 0, 4, 9, 4},
		{"shrink to min", 2, 2, 8, 0, 2},
		{"min floor", 2, 3, 8, 0, 3},
		{"default size one", 0, 0, 5, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adaptiveTarget(tc.size, tc.min, tc.max, tc.boost); got != tc.want {
				t.Fatalf("adaptiveTarget(%d,%d,%d,%d) = %d, want %d", tc.size, tc.min, tc.max, tc.boost, got, tc.want)
			}
		})
	}
}

func TestWarmDemand_GrowAndDecay(t *testing.T) {
	now := time.Now()
	d := &warmDemand{lastObserved: now}

	if got := d.observe(now, 0); got != 0 {
		t.Fatalf("no cold starts: want 0, got %d", got)
	}
	if got := d.observe(now, 3); got != 3 {
		t.Fatalf("burst of 3: want 3, got %d", got)
	}
	if got := d.observe(now.Add(time.Minute), 3); got != 3 {
		t.Fatalf("no new cold starts within window: want 3, got %d", got)
	}
	if got := d.observe(now.Add(6*time.Minute), 3); got != 0 {
		t.Fatalf("window elapsed without new cold starts: want decay to 0, got %d", got)
	}
	// a later burst restarts from the accumulated baseline
	if got := d.observe(now.Add(7*time.Minute), 5); got != 2 {
		t.Fatalf("new burst after decay: want 2, got %d", got)
	}
}

func TestComputeMaxPods_MultiNodeAndCPU(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()

	newTestNode(t, k8s, "node-a", "4Gi", "2")
	newTestNode(t, k8s, "node-b", "4Gi", "2")

	cfg := defaultCfg() // 500m CPU, 512Mi memory per pod
	// memCap = 2*4Gi*0.9/512Mi = 14; cpuCap = 2*2000m*0.9/500m = 7 → min = 7
	if got := computeMaxPods(ctx, k8s, cfg); got != 7 {
		t.Fatalf("expected min(mem, cpu) capacity = 7, got %d", got)
	}
}

func TestComputeMaxPods_NoNodes(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	if got := computeMaxPods(ctx, k8s, defaultCfg()); got != podCapacityUnknown {
		t.Fatalf("expected podCapacityUnknown without nodes, got %d", got)
	}
}

// newTestNode registers a node with the given allocatable memory/CPU.
func newTestNode(t *testing.T, k8s kubernetes.Interface, name, mem, cpu string) {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(mem),
			corev1.ResourceCPU:    resource.MustParse(cpu),
		}},
	}
	if _, err := k8s.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
}

// requestingPod builds a pod whose single container requests mem.
func requestingPod(ns, name, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "c",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)}},
		}}},
	}
}

// computeCapacity returns the capacity the reconciler would compute for a
// single node with the given allocatable, seeded with pods.
func computeCapacity(t *testing.T, mem, cpu string, pods ...*corev1.Pod) int64 {
	t.Helper()
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	newTestNode(t, k8s, "node-a", mem, cpu)
	seedPods(ctx, t, k8s, pods...)
	return computeMaxPods(ctx, k8s, defaultCfg())
}

// TestComputeMaxPods_SubtractsSystemWorkloads reproduces the t3.small incident:
// raw node math says three sandbox pods fit, but once the control-plane
// workloads holding their requests forever are subtracted only one fits. The
// old model ignored them and kept creating refills the scheduler could never
// place.
func TestComputeMaxPods_SubtractsSystemWorkloads(t *testing.T) {
	// 3 × cert-manager (128Mi) + coredns (70Mi) + metrics-server (200Mi) +
	// cilium (256Mi) = 910Mi held for the lifetime of the cluster.
	system := make([]*corev1.Pod, 0, 6)
	for name, mem := range map[string]string{
		"cert-manager":            "128Mi",
		"cert-manager-cainjector": "128Mi",
		"cert-manager-webhook":    "128Mi",
		"coredns":                 "70Mi",
		"metrics-server":          "200Mi",
		"cilium":                  "256Mi",
	} {
		system = append(system, requestingPod("kube-system", name, mem))
	}

	// ~1.9Gi allocatable: raw node math says three pods fit, but once the
	// control-plane requests are subtracted only one does.
	if got := computeCapacity(t, "1950944Ki", "2", system...); got != 1 {
		t.Fatalf("expected capacity 1 after subtracting system workloads, got %d", got)
	}
}

func TestComputeMaxPods_NoRoomLeft(t *testing.T) {
	filling := requestingPod("kube-system", "metrics-server", "900Mi")
	if got := computeCapacity(t, "1Gi", "1", filling); got != 0 {
		t.Fatalf("expected capacity 0 when system workloads fill the node, got %d", got)
	}
}

// TestComputeMaxPods_IgnoresSandboxPods guards against double counting: sandbox
// pods are charged against maxPods by count, not by request.
func TestComputeMaxPods_IgnoresSandboxPods(t *testing.T) {
	sandbox := requestingPod(testNS, "sandbox-warm-x", "512Mi")
	sandbox.Labels = map[string]string{sandboxgrpc.LabelState: sandboxgrpc.StateWarm}

	// memCap = 7, cpuCap = 3 → 3, identical to an empty cluster.
	if got := computeCapacity(t, "4Gi", "2", sandbox); got != 3 {
		t.Fatalf("expected sandbox pods excluded from capacity math, got %d", got)
	}
}

func TestComputeMaxPods_IgnoresCompletedPods(t *testing.T) {
	done := requestingPod("kube-system", "helm-install", "2Gi")
	done.Status.Phase = corev1.PodSucceeded

	if got := computeCapacity(t, "4Gi", "2", done); got != 3 {
		t.Fatalf("expected completed pods to release their requests, got %d", got)
	}
}

func TestRecycleUnhealthyWarmPods_UnschedulablePending(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	// Stuck unschedulable past the grace period — must be recycled so the next
	// refill can retry instead of waiting for the 2h idle reaper.
	stuck := aged(warmTestPod("warm-stuck", corev1.PodPending, condUnschedulable), 10*time.Minute)
	// Unschedulable, but only just: give the scheduler a chance to place it.
	recent := aged(warmTestPod("warm-recent", corev1.PodPending, condUnschedulable), 10*time.Second)
	// Pending for a long time but already bound: it is pulling the image.
	pulling := aged(warmTestPod("warm-pulling", corev1.PodPending, condScheduled), 10*time.Minute)

	seedPods(ctx, t, k8s, stuck, recent, pulling)

	recycleUnhealthyWarmPods(ctx, k8s, testNS)

	assertGone(t, k8s, "warm-stuck", "unschedulable past the grace period")
	assertPresent(t, k8s, "warm-recent")
	assertPresent(t, k8s, "warm-pulling")
}

// newWarmPoolCR builds a SandboxWarmPool CR with the given spec.
func newWarmPoolCR(size int64) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": sandboxgrpc.SandboxAPIGroup + "/v1alpha1",
		"kind":       "SandboxWarmPool",
		"metadata":   map[string]interface{}{"name": "default", "namespace": testNS},
		"spec":       map[string]interface{}{"size": size, "runtimeClass": "gvisor"},
	}}
}

// TestReconcileSinglePool_TrimsSurplusWarmPods covers scale-down: the pool asks
// for one pod but holds two (a session pod returning from resetting, or a
// lowered target). Surplus must go now, not at the next idle reaper sweep.
func TestReconcileSinglePool_TrimsSurplusWarmPods(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	ready := warmTestPod("warm-ready", corev1.PodRunning, condReady)
	stuck := warmTestPod("warm-stuck", corev1.PodPending, condUnschedulable)
	seedPods(ctx, t, k8s, ready, stuck)

	reconcileSinglePool(ctx, k8s, newWarmPoolCR(1), podCapacityUnknown, defaultCfg(), 0)

	// The unschedulable pod is worthless; the claimable one must stay.
	assertGone(t, k8s, "warm-stuck", "worthless to a session")
	assertPresent(t, k8s, "warm-ready")
}

func TestReconcileSinglePool_TrimPrefersLongestIdle(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()

	older := idleSince(warmTestPod("warm-old", corev1.PodRunning, condReady), 90*time.Minute)
	newer := idleSince(warmTestPod("warm-new", corev1.PodRunning, condReady), 5*time.Minute)
	seedPods(ctx, t, k8s, older, newer)

	reconcileSinglePool(ctx, k8s, newWarmPoolCR(1), podCapacityUnknown, defaultCfg(), 0)

	assertGone(t, k8s, "warm-old", "idled longest")
	assertPresent(t, k8s, "warm-new")
}

// TestReconcileSinglePool_RespectsCapacity is the incident fix: with one
// session already holding the only slot that fits, the reconciler must not
// create a refill the scheduler can never place.
func TestReconcileSinglePool_RespectsCapacity(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	active := sandboxTestPod("sandbox-warm-active", sandboxgrpc.StateActive, corev1.PodRunning)
	seedPods(ctx, t, k8s, active)

	reconcileSinglePool(ctx, k8s, newWarmPoolCR(2), 1, defaultCfg(), 0)

	pods, err := k8s.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected no refill once capacity is exhausted, got %d pods", len(pods.Items))
	}
}

// TestDeleteSurplusWarmPod_SkipsClaimed guards the trim/claim race: a session
// claiming a pod flips its label to active, and trimming it anyway would leave
// that session pointing at a dead sandbox.
func TestDeleteSurplusWarmPod_SkipsClaimed(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	pod := sandboxTestPod("warm-claimed", sandboxgrpc.StateActive, corev1.PodRunning)
	seedPods(ctx, t, k8s, pod)

	if deleteSurplusWarmPod(ctx, k8s, testNS, pod) {
		t.Fatal("claimed pod must not count against the trim budget")
	}
	assertPresent(t, k8s, "warm-claimed")
}

func TestCapacityCache_ReusesValue(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	newTestNode(t, k8s, "node-a", "4Gi", "2")

	caps := &capacityCache{}
	if got := caps.get(ctx, k8s, defaultCfg()); got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
	// Grow the cluster behind the cache's back: the cached value must hold
	// until the TTL expires.
	newTestNode(t, k8s, "node-b", "4Gi", "2")
	if got := caps.get(ctx, k8s, defaultCfg()); got != 3 {
		t.Fatalf("expected cached 3, got %d", got)
	}
	// A nil cache bypasses memoization entirely.
	if got := (*capacityCache)(nil).get(ctx, k8s, defaultCfg()); got != 7 {
		t.Fatalf("expected nil cache to recompute (7), got %d", got)
	}
}

func TestReapIfIdle_UsesPodTTLOverride(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()

	pod := idleSince(sandboxTestPod("warm-ttl", "", corev1.PodRunning), 2*time.Second)
	pod.Annotations[podIdleTTLAnnotation] = "1"
	seedPods(ctx, t, k8s, pod)

	// default TTL is long (3600s), but the pod annotation overrides it to 1s
	reapIfIdle(ctx, k8s, testNS, pod, 3600, time.Now())
	assertGone(t, k8s, "warm-ttl", "past its per-pod TTL override")
}

func TestReconcileWarmPools_NoPoolCR_CreatesNoPods(t *testing.T) {
	// Without a SandboxWarmPool CR the reconciler is a no-op (KIP-25).
	ctx := context.Background()
	dyn, k8s := fakeSandboxClients()
	reconcileWarmPools(ctx, k8s, dyn, defaultCfg(), nil, nil, nil)

	pods, err := k8s.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("expected 0 warm pods without a SandboxWarmPool CR, got %d", len(pods.Items))
	}
}

func TestWarmPoolReconciler_RefillTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dyn, k8s := fakeSandboxClients()

	pool := newWarmPoolCR(2)
	if _, err := dyn.Resource(warmPoolGVR).Namespace(testNS).Create(ctx, &pool, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create warm pool CR: %v", err)
	}

	refill := make(chan struct{}, 1)
	refill <- struct{}{}
	done := make(chan struct{})
	go func() {
		runWarmPoolReconciler(ctx, k8s, dyn, defaultCfg(), refill, nil, nil)
		close(done)
	}()

	// The refill signal must trigger a reconcile that creates the warm pod
	// without waiting for the 10s tick.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pods, _ := k8s.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
		if len(pods.Items) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	pods, err := k8s.CoreV1().Pods(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("expected refill trigger to create a warm pod immediately")
	}
}

func TestUpdateSandboxMatrixStatus_WritesMetrics(t *testing.T) {
	ctx := context.Background()
	dyn, k8s := fakeSandboxClients()
	ns := testNS

	matrix := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": sandboxgrpc.SandboxAPIGroup + "/v1alpha1",
		"kind":       "SandboxMatrix",
		"metadata":   map[string]interface{}{"name": "default", "namespace": ns},
	}}
	if _, err := dyn.Resource(localMatrixGVR).Namespace(ns).Create(ctx, matrix, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create matrix CR: %v", err)
	}

	orch := sandboxgrpc.NewOrchestrator(k8s, dyn, ns)
	updateSandboxMatrixStatus(ctx, k8s, dyn, defaultCfg(), orch, podCapacityUnknown)

	got, err := dyn.Resource(localMatrixGVR).Namespace(ns).Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get matrix: %v", err)
	}
	status, ok := got.Object["status"].(map[string]interface{})
	if !ok {
		t.Fatal("expected status on matrix CR")
	}
	for _, field := range []string{"claimedFromWarm", "coldStarts", "avgClaimLatencyMs", "readyWarmCount", "activeSessions", "maxPods", "totalPods"} {
		if _, present := status[field]; !present {
			t.Errorf("expected status field %s to be written", field)
		}
	}
}

func TestWarmPodSpec_RuntimeClass(t *testing.T) {
	spec := warmPodSpec("gvisor", defaultCfg())
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "gvisor" {
		t.Fatalf("expected runtimeClassName=gvisor, got %v", spec.RuntimeClassName)
	}
}

func TestWarmPodSpec_EmptyRuntimeClass(t *testing.T) {
	spec := warmPodSpec("", defaultCfg())
	if spec.RuntimeClassName != nil {
		t.Fatalf("expected nil runtimeClassName, got %v", spec.RuntimeClassName)
	}
}

func TestWarmPodSpec_Image(t *testing.T) {
	spec := warmPodSpec("gvisor", defaultCfg())
	if spec.Containers[0].Image != "ghcr.io/xiaods/k8e-sandbox:latest" {
		t.Fatalf("unexpected image: %s", spec.Containers[0].Image)
	}
}

func TestWarmPodSpec_Resources(t *testing.T) {
	spec := warmPodSpec("gvisor", defaultCfg())
	limits := spec.Containers[0].Resources.Limits
	if limits.Cpu().String() != "500m" {
		t.Fatalf("unexpected cpu: %s", limits.Cpu().String())
	}
	if limits.Memory().String() != "512Mi" {
		t.Fatalf("unexpected memory: %s", limits.Memory().String())
	}
}

func TestWarmPodSpec_CustomResources(t *testing.T) {
	cfg := defaultCfg()
	cfg.DefaultCPU = "2"
	cfg.DefaultMemory = "2Gi"
	spec := warmPodSpec("kata", cfg)
	if spec.Containers[0].Resources.Limits.Cpu().String() != "2" {
		t.Fatalf("unexpected cpu: %s", spec.Containers[0].Resources.Limits.Cpu().String())
	}
	if spec.Containers[0].Resources.Limits.Memory().String() != "2Gi" {
		t.Fatalf("unexpected memory: %s", spec.Containers[0].Resources.Limits.Memory().String())
	}
}

func TestWarmPodSpec_RestartPolicy(t *testing.T) {
	spec := warmPodSpec("gvisor", defaultCfg())
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("expected RestartPolicyNever, got %s", spec.RestartPolicy)
	}
}

func TestWarmPodSpec_SandboxdPort(t *testing.T) {
	spec := warmPodSpec("gvisor", defaultCfg())
	if len(spec.Containers[0].Ports) == 0 || spec.Containers[0].Ports[0].ContainerPort != 2024 {
		t.Fatalf("expected port 2024, got %v", spec.Containers[0].Ports)
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := config.SandboxConfig{}
	if cfg.DefaultRuntime == "" {
		cfg.DefaultRuntime = "gvisor"
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "ghcr.io/xiaods/k8e-sandbox:latest"
	}
	if cfg.DefaultCPU == "" {
		cfg.DefaultCPU = "500m"
	}
	if cfg.DefaultMemory == "" {
		cfg.DefaultMemory = "512Mi"
	}
	if cfg.GRPCPort == 0 {
		cfg.GRPCPort = 50051
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "sandbox-matrix"
	}

	if cfg.DefaultRuntime != "gvisor" {
		t.Errorf("DefaultRuntime: got %s", cfg.DefaultRuntime)
	}
	if cfg.DefaultImage != "ghcr.io/xiaods/k8e-sandbox:latest" {
		t.Errorf("DefaultImage: got %s", cfg.DefaultImage)
	}
	if cfg.GRPCPort != 50051 {
		t.Errorf("GRPCPort: got %d", cfg.GRPCPort)
	}
	if cfg.Namespace != "sandbox-matrix" {
		t.Errorf("Namespace: got %s", cfg.Namespace)
	}
}
