// Package sandboxmatrix implements the Agentic AI Sandbox Matrix controller.
package sandboxmatrix

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/xiaods/k8e/pkg/daemons/config"
	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
)

var warmPoolGVR = schema.GroupVersionResource{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxwarmpools"}
var localMatrixGVR = schema.GroupVersionResource{Group: sandboxgrpc.SandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxmatrices"}

const tlsDir = "/var/lib/k8e/server/tls"

// Register starts the SandboxMatrix controller and gRPC gateway.
func Register(ctx context.Context, k8s kubernetes.Interface, kubeconfig string, cfg config.SandboxConfig) error {
	// Apply defaults
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

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	// refillTrigger wakes the warm pool reconciler immediately after a warm pod
	// claim, instead of waiting up to 10s for the next poll tick.
	refillTrigger := make(chan struct{}, 1)
	orch := sandboxgrpc.NewOrchestrator(k8s, dyn)
	orch.OnWarmClaim = func() {
		select {
		case refillTrigger <- struct{}{}:
		default: // already a refill pending
		}
	}
	go runWarmPoolReconciler(ctx, k8s, dyn, cfg, refillTrigger, orch)
	go runResettingDetector(ctx, k8s, cfg.Namespace)
	go runIdlePodReaper(ctx, k8s, dyn, cfg)

	go runGCLoop(ctx, orch, cfg.Namespace)

	srv := sandboxgrpc.NewServer(sandboxgrpc.ServerConfig{
		K8s:            k8s,
		Dyn:            dyn,
		CACertFile:     tlsDir + "/sandbox-ca.crt",
		CAKeyFile:      tlsDir + "/sandbox-ca.key",
		ServerCertFile: tlsDir + "/sandbox-server.crt",
		ServerKeyFile:  tlsDir + "/sandbox-server.key",
		GRPCPort:       cfg.GRPCPort,
	})
	go func() {
		if err := srv.Start(ctx); err != nil {
			logrus.Errorf("sandbox gRPC gateway: %v", err)
		}
	}()

	if _, err := os.Stat("/dev/kvm"); err == nil {
		logrus.Info("sandbox-matrix: /dev/kvm detected, Firecracker RuntimeClass enabled")
	} else {
		logrus.Info("sandbox-matrix: /dev/kvm not found, Firecracker RuntimeClass skipped")
	}

	logrus.Infof("sandbox-matrix: controller started (runtime=%s namespace=%s grpc-port=%d)",
		cfg.DefaultRuntime, cfg.Namespace, cfg.GRPCPort)
	return nil
}

func runWarmPoolReconciler(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig, refill <-chan struct{}, orch *sandboxgrpc.Orchestrator) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	demand := &warmDemand{lastObserved: time.Now()}
	for {
		select {
		case <-ctx.Done():
			return
		case <-refill:
			// A session just claimed a warm pod; refill before the next tick.
			reconcileWarmPools(ctx, k8s, dyn, cfg, orch, demand)
		case <-ticker.C:
			reconcileWarmPools(ctx, k8s, dyn, cfg, orch, demand)
		}
	}
}

func reconcileWarmPools(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig, orch *sandboxgrpc.Orchestrator, demand *warmDemand) {
	pools, err := dyn.Resource(warmPoolGVR).Namespace(cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	maxPods := computeMaxPods(ctx, k8s, cfg)
	boost := int64(0)
	if orch != nil && demand != nil {
		_, coldStarts, _ := orch.Metrics()
		boost = demand.observe(time.Now(), coldStarts)
	}
	for _, pool := range pools.Items {
		reconcileSinglePool(ctx, k8s, pool, maxPods, cfg, boost)
	}
	recycleUnhealthyWarmPods(ctx, k8s, cfg.Namespace)
	updateSandboxMatrixStatus(ctx, k8s, dyn, cfg, orch)
}

// demandDecayWindow is how long without a cold start before the adaptive pool
// target decays back toward MinSize.
const demandDecayWindow = 5 * time.Minute

// warmDemand tracks recent cold starts so the warm pool can grow on bursts and
// shrink once demand subsides.
type warmDemand struct {
	mu               sync.Mutex
	lastColdStarts   int64
	lastObserved     time.Time
	recentColdStarts int64
}

// observe folds new cold starts into the demand estimate. When no new cold
// starts occur for demandDecayWindow, the estimate decays back to zero.
func (d *warmDemand) observe(now time.Time, coldStarts int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	delta := coldStarts - d.lastColdStarts
	if delta > 0 {
		d.recentColdStarts += delta
		d.lastColdStarts = coldStarts
		d.lastObserved = now
	} else if now.Sub(d.lastObserved) >= demandDecayWindow {
		d.recentColdStarts = 0
		d.lastObserved = now
	}
	return d.recentColdStarts
}

// recycleUnhealthyWarmPodAfter is how long a Running warm pod may stay not-ready
// (sandboxd not serving on :2024) before the reconciler recycles it so a fresh
// pod is created in its place.
const recycleUnhealthyWarmPodAfter = 5 * time.Minute

// recycleUnhealthyWarmPods deletes warm pods whose sandboxd is not serving:
// Failed pods (container exited; RestartPolicy Never leaves the pod Failed) and
// Running pods stuck without the Ready condition for longer than
// recycleUnhealthyWarmPodAfter. The reconciler recreates them on the next tick,
// keeping the pool at target without burning memory on dead pods.
func recycleUnhealthyWarmPods(ctx context.Context, k8s kubernetes.Interface, namespace string) {
	pods, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateWarm,
	})
	if err != nil {
		return
	}
	now := time.Now()
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodFailed {
			logrus.Infof("sandbox-matrix: recycle failed warm pod %s", pod.Name)
			deleteWarmPod(ctx, k8s, namespace, pod.Name)
			continue
		}
		if pod.Status.Phase != corev1.PodRunning || sandboxgrpc.PodReadyCondition(pod) {
			continue
		}
		if now.Sub(pod.CreationTimestamp.Time) < recycleUnhealthyWarmPodAfter {
			continue // still within the boot budget (image pull + sandboxd start)
		}
		logrus.Infof("sandbox-matrix: recycle warm pod %s (sandboxd not ready for %v)",
			pod.Name, now.Sub(pod.CreationTimestamp.Time).Round(time.Second))
		deleteWarmPod(ctx, k8s, namespace, pod.Name)
	}
}

func deleteWarmPod(ctx context.Context, k8s kubernetes.Interface, namespace, name string) {
	if err := k8s.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		logrus.Warnf("sandbox-matrix: recycle delete %s: %v", name, err)
	}
}

// reconcileSinglePool ensures one WarmPool CRD's target is met within capacity limits.
func reconcileSinglePool(ctx context.Context, k8s kubernetes.Interface, pool unstructured.Unstructured, maxPods int64, cfg config.SandboxConfig, boost int64) {
	specMap, _ := pool.Object["spec"].(map[string]interface{})
	configuredSize, _ := specMap["size"].(int64)
	runtimeClass, _ := specMap["runtimeClass"].(string)
	if runtimeClass == "" {
		runtimeClass = cfg.DefaultRuntime
	}
	minSize, _ := specMap["minSize"].(int64)
	maxSize, _ := specMap["maxSize"].(int64)
	idleTTL, _ := specMap["idleTTLSeconds"].(int64)

	targetSize := poolTargetSize(adaptiveTarget(configuredSize, minSize, maxSize, boost), maxPods)

	allPods, err := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxgrpc.LabelState,
	})
	if err != nil {
		return
	}
	if maxPods > 0 && int64(len(allPods.Items)) >= maxPods {
		return
	}

	warmPods, err := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateWarm,
	})
	if err != nil {
		return
	}

	gap := targetSize - int64(len(warmPods.Items))
	for i := int64(0); i < gap; i++ {
		if maxPods > 0 && int64(len(allPods.Items))+i+1 > maxPods {
			break
		}
		pod := newWarmPod(cfg, runtimeClass, idleTTL)
		if _, err := k8s.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			logrus.Debugf("sandbox-matrix: create warm pod: %v", err)
		}
	}
}

// adaptiveTarget computes the warm pool target for this reconcile. Adaptive
// mode is opt-in via MaxSize > Size: the target starts at Size, grows to cover
// recent cold starts (bounded by MaxSize and capacity), and never drops below
// MinSize (defaulting to Size). Without MaxSize the target is static.
func adaptiveTarget(configuredSize, minSize, maxSize, boost int64) int64 {
	base := configuredSize
	if base <= 0 {
		base = 1
	}
	if maxSize <= base {
		return base
	}
	lo := minSize
	if lo <= 0 {
		lo = base
	}
	target := base
	if boost > target {
		target = boost
	}
	if target < lo {
		target = lo
	}
	if target > maxSize {
		target = maxSize
	}
	return target
}

// poolTargetSize computes the warm pool target bounded by capacity.
func poolTargetSize(configured, maxPods int64) int64 {
	t := configured
	if t <= 0 {
		t = 1
	}
	if maxPods > 0 && t > maxPods {
		t = maxPods
	}
	if t < 1 {
		t = 1
	}
	return t
}

// podIdleTTLAnnotation records a per-pool idle TTL override on warm pods.
const podIdleTTLAnnotation = "sandbox.k8e.io/idle-ttl-seconds"

// newWarmPod creates a warm pod spec with the correct labels and runtime.
// idleTTLSeconds > 0 stamps the per-pool idle TTL override onto the pod.
func newWarmPod(cfg config.SandboxConfig, runtimeClass string, idleTTLSeconds int64) *corev1.Pod {
	annotations := sandboxgrpc.GvisorAnnotations(runtimeClass)
	if idleTTLSeconds > 0 {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[podIdleTTLAnnotation] = strconv.FormatInt(idleTTLSeconds, 10)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sandbox-warm-",
			Namespace:    cfg.Namespace,
			Labels: map[string]string{
				sandboxgrpc.LabelState:        sandboxgrpc.StateWarm,
				sandboxgrpc.LabelRuntimeClass: runtimeClass,
			},
			Annotations: annotations,
		},
		Spec: warmPodSpec(runtimeClass, cfg),
	}
}

// computeMaxPods returns the maximum number of sandbox pods the cluster can
// host, summing allocatable memory and CPU across all nodes (10% buffer each)
// and taking the tighter bound. Returns 0 if node metrics are unavailable
// (no limit enforced).
func computeMaxPods(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig) int64 {
	nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return 0
	}

	var totalMem, totalCPU int64
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if mem := node.Status.Allocatable.Memory(); mem != nil {
			totalMem += mem.Value()
		}
		if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
			totalCPU += cpu.MilliValue() // millicores
		}
	}
	if totalMem == 0 && totalCPU == 0 {
		return 0
	}

	perPodMem := resource.MustParse(cfg.DefaultMemory)
	if perPodMem.IsZero() {
		perPodMem = resource.MustParse("512Mi")
	}
	perPodCPU := resource.MustParse(cfg.DefaultCPU)
	if perPodCPU.IsZero() {
		perPodCPU = resource.MustParse("500m")
	}

	memCap := int64(0)
	if totalMem > 0 && !perPodMem.IsZero() {
		memCap = totalMem * 9 / 10 / perPodMem.Value()
	}
	cpuCap := int64(0)
	if totalCPU > 0 && !perPodCPU.IsZero() {
		cpuCap = totalCPU * 9 / 10 / perPodCPU.MilliValue()
	}

	if memCap == 0 {
		return cpuCap
	}
	if cpuCap == 0 {
		return memCap
	}
	if memCap < cpuCap {
		return memCap
	}
	return cpuCap
}

func updateSandboxMatrixStatus(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig, orch *sandboxgrpc.Orchestrator) {
	matrices, err := dyn.Resource(localMatrixGVR).Namespace(cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(matrices.Items) == 0 {
		return
	}

	warmPods, _ := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateWarm})
	activePods, _ := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateActive})

	readyWarm := 0
	for i := range warmPods.Items {
		if warmPods.Items[i].Status.Phase == corev1.PodRunning {
			readyWarm++
		}
	}

	maxPods := computeMaxPods(ctx, k8s, cfg)
	totalPods := int64(len(warmPods.Items) + len(activePods.Items))

	claimedWarm, coldStarts, avgLatency := int64(0), int64(0), int64(0)
	if orch != nil {
		claimedWarm, coldStarts, avgLatency = orch.Metrics()
	}

	matrix := matrices.Items[0].DeepCopy()
	if matrix.Object["status"] == nil {
		matrix.Object["status"] = map[string]interface{}{}
	}
	status := matrix.Object["status"].(map[string]interface{})
	status["readyWarmCount"] = int64(readyWarm)
	status["activeSessions"] = int64(len(activePods.Items))
	status["maxPods"] = maxPods
	status["totalPods"] = totalPods
	status["claimedFromWarm"] = claimedWarm
	status["coldStarts"] = coldStarts
	status["avgClaimLatencyMs"] = avgLatency
	dyn.Resource(localMatrixGVR).Namespace(cfg.Namespace).UpdateStatus(ctx, matrix, metav1.UpdateOptions{}) //nolint:errcheck
}

func warmPodSpec(runtimeClass string, cfg config.SandboxConfig) corev1.PodSpec {
	// Warm pool pods use EmptyDir (no PVC) — pass empty pvcName
	return sandboxgrpc.SandboxPodSpec(runtimeClass, "" /* no PVC */, cfg.DefaultCPU, cfg.DefaultMemory, cfg.DefaultImage)
}

func runGCLoop(ctx context.Context, orch *sandboxgrpc.Orchestrator, namespace string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gcExpiredSessions(ctx, orch, namespace)
		}
	}
}

func gcExpiredSessions(ctx context.Context, orch *sandboxgrpc.Orchestrator, namespace string) {
	sessions, err := orch.ListActiveSessions(ctx, namespace)
	if err != nil {
		return
	}
	now := time.Now()
	for _, s := range sessions {
		if s.Status.ExpiresAt != nil && s.Status.ExpiresAt.Time.Before(now) {
			logrus.Infof("sandbox-matrix: GC session %s (expired at %s)", s.Name, s.Status.ExpiresAt.Time)
			if err := orch.DestroySession(ctx, s.Name); err != nil {
				logrus.Warnf("sandbox-matrix: GC destroy %s: %v", s.Name, err)
			}
		}
	}
}

// podReleasedAtAnnotation records when a pod was released back to warm pool.
const podReleasedAtAnnotation = "sandbox.k8e.io/released-at"

// runResettingDetector watches pods in resetting state and promotes them to warm once ready.
func runResettingDetector(ctx context.Context, k8s kubernetes.Interface, namespace string) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			detectResetting(ctx, k8s, namespace)
		}
	}
}

func detectResetting(ctx context.Context, k8s kubernetes.Interface, namespace string) {
	pods, err := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateResetting,
	})
	if err != nil {
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		// Pod is running — workspace reset should be complete by now.
		// Promote to warm with release timestamp.
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[sandboxgrpc.LabelState] = sandboxgrpc.StateWarm
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[podReleasedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
		_, updateErr := k8s.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{})
		if updateErr == nil {
			logrus.Debugf("sandbox-matrix: pod %s promoted resetting → warm", pod.Name)
		}
	}
}

// runIdlePodReaper destroys warm pods that have been idle longer than TTL.
func runIdlePodReaper(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapIdlePods(ctx, k8s, dyn, cfg)
		}
	}
}

func reapIdlePods(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig) {
	ttl := getSessionTTL(ctx, dyn, cfg.Namespace) * 2 // idle TTL = sessionTTL × 2

	pods, err := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateWarm,
	})
	if err != nil {
		return
	}

	now := time.Now()
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ensureReleaseTimestamp(ctx, k8s, cfg.Namespace, pod, now) {
			continue
		}
		reapIfIdle(ctx, k8s, cfg.Namespace, pod, ttl, now)
	}
}

// getSessionTTL reads the session TTL from the SandboxMatrix CRD. Returns 3600 as default.
func getSessionTTL(ctx context.Context, dyn dynamic.Interface, namespace string) int64 {
	matrices, err := dyn.Resource(localMatrixGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(matrices.Items) == 0 {
		return 3600
	}
	if ttlVal, found, _ := unstructured.NestedInt64(matrices.Items[0].Object, "spec", "sessionTTL"); found {
		return ttlVal
	}
	return 3600
}

// ensureReleaseTimestamp adds a released-at annotation if missing, returns false if pod should be skipped.
func ensureReleaseTimestamp(ctx context.Context, k8s kubernetes.Interface, namespace string, pod *corev1.Pod, now time.Time) bool {
	if pod.Annotations[podReleasedAtAnnotation] != "" {
		return true
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[podReleasedAtAnnotation] = now.UTC().Format(time.RFC3339)
	k8s.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{}) //nolint:errcheck
	return false
}

// reapIfIdle deletes the pod if it has been idle longer than the TTL. A
// per-pool override on the pod (idle-ttl-seconds annotation) takes precedence
// over the default (sessionTTL × 2).
func reapIfIdle(ctx context.Context, k8s kubernetes.Interface, namespace string, pod *corev1.Pod, defaultTTL int64, now time.Time) {
	ttl := defaultTTL
	if a := pod.Annotations[podIdleTTLAnnotation]; a != "" {
		if v, err := strconv.ParseInt(a, 10, 64); err == nil && v > 0 {
			ttl = v
		}
	}
	releasedAt := pod.Annotations[podReleasedAtAnnotation]
	t, parseErr := time.Parse(time.RFC3339, releasedAt)
	if parseErr != nil {
		return
	}
	if now.Sub(t) <= time.Duration(ttl)*time.Second {
		return
	}
	logrus.Infof("sandbox-matrix: reap idle pod %s (idle %v, ttl %ds)", pod.Name, now.Sub(t).Round(time.Second), ttl)
	if delErr := k8s.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); delErr != nil {
		logrus.Warnf("sandbox-matrix: reap delete %s: %v", pod.Name, delErr)
	}
}
