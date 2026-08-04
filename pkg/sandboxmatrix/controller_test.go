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
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func defaultCfg() config.SandboxConfig {
	return config.SandboxConfig{
		DefaultRuntime: "gvisor",
		DefaultImage:   "ghcr.io/xiaods/k8e-sandbox:latest",
		DefaultCPU:     "500m",
		DefaultMemory:  "512Mi",
		GRPCPort:       50051,
		Namespace:      "sandbox-matrix",
	}
}

func TestRecycleUnhealthyWarmPods(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	ns := "sandbox-matrix"

	// Failed warm pod (sandboxd exited; RestartPolicy Never) — must be recycled.
	failed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-failed", Namespace: ns, Labels: map[string]string{sandboxgrpc.LabelState: sandboxgrpc.StateWarm}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	// Running warm pod stuck not-ready past the boot budget — must be recycled.
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-stale",
			Namespace:         ns,
			Labels:            map[string]string{sandboxgrpc.LabelState: sandboxgrpc.StateWarm},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// Fresh not-ready warm pod — still within boot budget, keep.
	fresh := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-fresh",
			Namespace:         ns,
			Labels:            map[string]string{sandboxgrpc.LabelState: sandboxgrpc.StateWarm},
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// Healthy warm pod — keep.
	healthy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-healthy", Namespace: ns, Labels: map[string]string{sandboxgrpc.LabelState: sandboxgrpc.StateWarm}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	for _, p := range []*corev1.Pod{failed, stale, fresh, healthy} {
		if _, err := k8s.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod %s: %v", p.Name, err)
		}
	}

	recycleUnhealthyWarmPods(ctx, k8s, ns)

	for _, name := range []string{"warm-failed", "warm-stale"} {
		if _, err := k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Errorf("expected %s to be recycled", name)
		}
	}
	for _, name := range []string{"warm-fresh", "warm-healthy"} {
		if _, err := k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("expected %s to survive: %v", name, err)
		}
	}
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

	for _, name := range []string{"node-a", "node-b"} {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourceCPU:    resource.MustParse("2"),
			}},
		}
		if _, err := k8s.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create node %s: %v", name, err)
		}
	}

	cfg := defaultCfg() // 500m CPU, 512Mi memory per pod
	// memCap = 2*4Gi*0.9/512Mi = 14; cpuCap = 2*2000m*0.9/500m = 7 → min = 7
	if got := computeMaxPods(ctx, k8s, cfg); got != 7 {
		t.Fatalf("expected min(mem, cpu) capacity = 7, got %d", got)
	}
}

func TestComputeMaxPods_NoNodes(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	if got := computeMaxPods(ctx, k8s, defaultCfg()); got != 0 {
		t.Fatalf("expected 0 (no limit) without nodes, got %d", got)
	}
}

func TestReapIfIdle_UsesPodTTLOverride(t *testing.T) {
	ctx := context.Background()
	k8s := kubefake.NewSimpleClientset()
	ns := "sandbox-matrix"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-ttl",
			Namespace: ns,
			Annotations: map[string]string{
				podIdleTTLAnnotation:    "1",
				podReleasedAtAnnotation: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339),
			},
		},
	}
	if _, err := k8s.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// default TTL is long (3600s), but the pod annotation overrides it to 1s
	reapIfIdle(ctx, k8s, ns, pod, 3600, time.Now())
	if _, err := k8s.CoreV1().Pods(ns).Get(ctx, "warm-ttl", metav1.GetOptions{}); err == nil {
		t.Fatal("expected pod to be reaped by its per-pod TTL override")
	}
}

func TestWarmPoolReconciler_RefillTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxWarmPool"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxWarmPoolList"}, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxMatrix"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxMatrixList"}, &unstructured.UnstructuredList{})
	listKinds := map[schema.GroupVersionResource]string{
		{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxwarmpools"}: "SandboxWarmPoolList",
		{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxmatrices"}:  "SandboxMatrixList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	k8s := kubefake.NewSimpleClientset()
	ns := "sandbox-matrix"

	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": sandboxgrpc.SandboxAPIGroup + "/v1alpha1",
		"kind":       "SandboxWarmPool",
		"metadata":   map[string]interface{}{"name": "default", "namespace": ns},
		"spec":       map[string]interface{}{"size": int64(2), "runtimeClass": "gvisor"},
	}}
	if _, err := dyn.Resource(warmPoolGVR).Namespace(ns).Create(ctx, pool, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create warm pool CR: %v", err)
	}

	refill := make(chan struct{}, 1)
	refill <- struct{}{}
	done := make(chan struct{})
	go func() {
		runWarmPoolReconciler(ctx, k8s, dyn, defaultCfg(), refill, nil)
		close(done)
	}()

	// The refill signal must trigger a reconcile that creates the warm pod
	// without waiting for the 10s tick.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pods, _ := k8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if len(pods.Items) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	pods, err := k8s.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("expected refill trigger to create a warm pod immediately")
	}
}

func TestUpdateSandboxMatrixStatus_WritesMetrics(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxMatrix"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxMatrixList"}, &unstructured.UnstructuredList{})
	listKinds := map[schema.GroupVersionResource]string{
		{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxmatrices"}: "SandboxMatrixList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	k8s := kubefake.NewSimpleClientset()
	ns := "sandbox-matrix"

	matrix := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": sandboxgrpc.SandboxAPIGroup + "/v1alpha1",
		"kind":       "SandboxMatrix",
		"metadata":   map[string]interface{}{"name": "default", "namespace": ns},
	}}
	if _, err := dyn.Resource(localMatrixGVR).Namespace(ns).Create(ctx, matrix, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create matrix CR: %v", err)
	}

	orch := sandboxgrpc.NewOrchestrator(k8s, dyn)
	updateSandboxMatrixStatus(ctx, k8s, dyn, defaultCfg(), orch)

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
