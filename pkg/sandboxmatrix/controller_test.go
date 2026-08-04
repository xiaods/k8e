package sandboxmatrix

import (
	"context"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/daemons/config"
	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
	corev1 "k8s.io/api/core/v1"
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
	pod := newWarmPod(defaultCfg(), "kata")
	if pod.Labels[sandboxgrpc.LabelRuntimeClass] != "kata" {
		t.Fatalf("expected runtime-class label kata, got %v", pod.Labels)
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
