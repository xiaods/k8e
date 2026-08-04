package sandboxmatrix

import (
	"context"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/daemons/config"
	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
