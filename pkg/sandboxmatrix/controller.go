// Package sandboxmatrix implements the Agentic AI Sandbox Matrix controller.
package sandboxmatrix

import (
	"context"
	"os"
	"sort"
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
		cfg.Namespace = config.DefaultSandboxNamespace
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
	orch := sandboxgrpc.NewOrchestrator(k8s, dyn, cfg.Namespace)
	orch.OnWarmClaim = func() {
		select {
		case refillTrigger <- struct{}{}:
		default: // already a refill pending
		}
	}

	// The gRPC gateway is stateless and multi-node-safe: start it on every
	// server. The reconcilers (warm pool, idle reaper, resetting detector,
	// GC) are NOT safe to run on every HA control-plane node — two servers
	// reconciling the same pool would double-create/double-GC/double-reap —
	// so they run only on the leader elected via a coordination Lease.
	srv := sandboxgrpc.NewServer(sandboxgrpc.ServerConfig{
		K8s:               k8s,
		Dyn:               dyn,
		Namespace:         cfg.Namespace,
		CACertFile:        tlsDir + "/sandbox-ca.crt",
		CAKeyFile:         tlsDir + "/sandbox-ca.key",
		ServerCertFile:    tlsDir + "/sandbox-server.crt",
		ServerKeyFile:     tlsDir + "/sandbox-server.key",
		GRPCPort:          cfg.GRPCPort,
		LayerStoreDir:     cfg.LayerStoreDir,
		FQDNEnabled:       cfg.CiliumDNSProxyEnabled,
		AdvertiseHostname: cfg.AdvertiseHostname,
		ExposeBaseURL:     cfg.ExposeBaseURL,
		AdvertiseIP:       cfg.AdvertiseIP,
		// The embedded e2b server (same host process) dials the gateway over
		// loopback with CA trust but NO client certificate (see
		// runEmbeddedE2B / newLocalClient). LocalAuth permits exactly those
		// loopback connections; every remote peer still requires full mTLS.
		LocalAuth: true,
	})
	go func() {
		if err := srv.Start(ctx); err != nil {
			logrus.Errorf("sandbox gRPC gateway: %v", err)
		}
	}()

	caps := &capacityCache{}
	go runLeaderGated(ctx, k8s, cfg, func(leaderCtx context.Context) {
		// Each reconciler is a blocking loop; start them concurrently so a
		// leader runs all four.
		go runWarmPoolReconciler(leaderCtx, k8s, dyn, cfg, refillTrigger, orch, caps)
		go runResettingDetector(leaderCtx, k8s, cfg.Namespace)
		go runIdlePodReaper(leaderCtx, k8s, dyn, cfg)
		go runGCLoop(leaderCtx, orch, cfg.Namespace)
	})

	if _, err := os.Stat("/dev/kvm"); err == nil {
		logrus.Info("sandbox-matrix: /dev/kvm detected, Firecracker RuntimeClass enabled")
	} else {
		logrus.Info("sandbox-matrix: /dev/kvm not found, Firecracker RuntimeClass skipped")
	}

	logrus.Infof("sandbox-matrix: controller started (runtime=%s namespace=%s grpc-port=%d leader-election=%s)",
		cfg.DefaultRuntime, cfg.Namespace, cfg.GRPCPort, leaderElectionLeaseName)
	return nil
}

func runWarmPoolReconciler(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig, refill <-chan struct{}, orch *sandboxgrpc.Orchestrator, caps *capacityCache) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	demand := &warmDemand{lastObserved: time.Now()}
	for {
		select {
		case <-ctx.Done():
			return
		case <-refill:
			// A session just claimed a warm pod; refill before the next tick.
			reconcileWarmPools(ctx, k8s, dyn, cfg, orch, demand, caps)
		case <-ticker.C:
			reconcileWarmPools(ctx, k8s, dyn, cfg, orch, demand, caps)
		}
	}
}

func reconcileWarmPools(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig, orch *sandboxgrpc.Orchestrator, demand *warmDemand, caps *capacityCache) {
	pools, err := dyn.Resource(warmPoolGVR).Namespace(cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	maxPods := caps.get(ctx, k8s, cfg)
	boost := int64(0)
	if orch != nil && demand != nil {
		_, coldStarts, _ := orch.Metrics()
		boost = demand.observe(time.Now(), coldStarts)
	}
	for _, pool := range pools.Items {
		reconcileSinglePool(ctx, k8s, pool, maxPods, cfg, boost)
	}
	recycleUnhealthyWarmPods(ctx, k8s, cfg.Namespace)
	updateSandboxMatrixStatus(ctx, k8s, dyn, cfg, orch, maxPods)
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

// recycleUnschedulableWarmPodAfter is how long a warm pod may sit Pending with
// the scheduler reporting it Unschedulable before the reconciler recycles it.
// Short enough that a refill is retried quickly once capacity frees up, long
// enough that transient scheduler pressure does not thrash the pool.
const recycleUnschedulableWarmPodAfter = 2 * time.Minute

// recycleUnhealthyWarmPods deletes warm pods that will never serve a session:
// Failed pods (container exited; RestartPolicy Never leaves the pod Failed),
// Running pods stuck without the Ready condition for longer than
// recycleUnhealthyWarmPodAfter, and Pending pods the scheduler has already
// given up on (see recycleUnschedulableWarmPodAfter). The reconciler recreates
// them on the next tick, keeping the pool at target without burning memory on
// dead pods.
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
		reason, age := warmPodRecycleReason(pod, now)
		if reason == "" {
			continue
		}
		logrus.Infof("sandbox-matrix: recycle warm pod %s (%s for %v)", pod.Name, reason, age.Round(time.Second))
		deleteWarmPod(ctx, k8s, namespace, pod.Name)
	}
}

// warmPodRecycleReason reports why a warm pod will never serve a session, or ""
// when it is still worth keeping. age is how long that reason has held.
func warmPodRecycleReason(pod *corev1.Pod, now time.Time) (string, time.Duration) {
	age := now.Sub(pod.CreationTimestamp.Time)
	switch pod.Status.Phase {
	case corev1.PodFailed:
		// RestartPolicy Never: the container exited and nothing will restart it.
		return "container exited", age
	case corev1.PodPending:
		// A pod the scheduler cannot place never becomes claimable and never
		// fails on its own. Left alone it would sit in the pool until the idle
		// reaper's sessionTTL×2 sweep, blocking the slot a refill needs and
		// turning into a delete/recreate cycle every two hours.
		if !podUnschedulable(pod) {
			return "", 0 // still waiting on image pull or an in-flight binding
		}
		if age < recycleUnschedulableWarmPodAfter {
			return "", 0 // give the scheduler a grace period to place it
		}
		return "unschedulable", age
	case corev1.PodRunning:
		if sandboxgrpc.PodReadyCondition(pod) {
			return "", 0
		}
		if age < recycleUnhealthyWarmPodAfter {
			return "", 0 // still within the boot budget (image pull + sandboxd start)
		}
		return "sandboxd not ready", age
	}
	return "", 0 // Succeeded, Unknown, …
}

// podUnschedulable reports whether the scheduler has given up on placing the
// pod. A Pending pod that is merely pulling an image keeps PodScheduled=True,
// so this only matches real placement failures: insufficient cpu/memory, an
// unsatisfiable node selector, an unbound PVC, and so on.
func podUnschedulable(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return c.Reason == corev1.PodReasonUnschedulable
		}
	}
	return false
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

	warmPods, err := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxgrpc.LabelState + "=" + sandboxgrpc.StateWarm,
	})
	if err != nil {
		return
	}

	gap := targetSize - int64(len(warmPods.Items))
	switch {
	case gap < 0:
		// The pool holds more warm pods than it asked for — session pods
		// returning from resetting, or a target that was just lowered. Scale
		// down now instead of waiting for the idle reaper's sessionTTL×2 sweep.
		trimWarmPods(ctx, k8s, cfg.Namespace, warmPods.Items, -gap)
		return
	case gap == 0:
		return
	}
	if maxPods >= 0 && int64(len(allPods.Items)) >= maxPods {
		return
	}
	for i := int64(0); i < gap; i++ {
		if maxPods >= 0 && int64(len(allPods.Items))+i+1 > maxPods {
			break
		}
		pod := newWarmPod(cfg, runtimeClass, idleTTL)
		if _, err := k8s.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			logrus.Debugf("sandbox-matrix: create warm pod: %v", err)
		}
	}
}

// trimWarmPods deletes up to count surplus warm pods, least valuable first:
// pods the scheduler could not place or that never became ready, then the pod
// that has been idle longest. Running+Ready pods go last because they are the
// ones a session can claim immediately.
func trimWarmPods(ctx context.Context, k8s kubernetes.Interface, namespace string, pods []corev1.Pod, count int64) {
	if count <= 0 {
		return
	}
	ordered := make([]*corev1.Pod, 0, len(pods))
	for i := range pods {
		ordered = append(ordered, &pods[i])
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := warmPodTrimPriority(ordered[i]), warmPodTrimPriority(ordered[j])
		if pi != pj {
			return pi > pj // higher priority is trimmed first
		}
		return podIdleSince(ordered[i]).Before(podIdleSince(ordered[j]))
	})
	trimmed := int64(0)
	for _, pod := range ordered {
		if trimmed >= count {
			return
		}
		if deleteSurplusWarmPod(ctx, k8s, namespace, pod) {
			trimmed++
		}
	}
}

// warmPodTrimPriority ranks how eagerly a warm pod may be trimmed. Anything a
// session cannot claim outranks a healthy pod.
func warmPodTrimPriority(pod *corev1.Pod) int {
	switch {
	case pod.Status.Phase != corev1.PodRunning:
		return 3 // Pending/Unschedulable, Failed, Unknown: never claimable
	case !sandboxgrpc.PodReadyCondition(pod):
		return 2 // running, but sandboxd is not serving on :2024 yet
	default:
		return 1
	}
}

// podIdleSince returns when the pod went back to the warm pool, falling back to
// its creation time for pods a session never released.
func podIdleSince(pod *corev1.Pod) time.Time {
	if t, err := time.Parse(time.RFC3339, pod.Annotations[podReleasedAtAnnotation]); err == nil {
		return t
	}
	return pod.CreationTimestamp.Time
}

// deleteSurplusWarmPod deletes one warm pod and reports whether it counted
// against the trim budget. The pod is re-read first: a session claiming it
// flips the state label to active, and deleting a freshly claimed pod would
// leave that session pointing at a dead sandbox.
func deleteSurplusWarmPod(ctx context.Context, k8s kubernetes.Interface, namespace string, pod *corev1.Pod) bool {
	current, err := k8s.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return false // already gone
	}
	if current.Labels[sandboxgrpc.LabelState] != sandboxgrpc.StateWarm {
		return false
	}
	if err := k8s.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			logrus.Warnf("sandbox-matrix: trim warm pod %s: %v", pod.Name, err)
		}
		return false
	}
	logrus.Infof("sandbox-matrix: trim surplus warm pod %s (pool above target)", pod.Name)
	return true
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
// maxPods == podCapacityUnknown disables the bound.
func poolTargetSize(configured, maxPods int64) int64 {
	t := configured
	if t <= 0 {
		t = 1
	}
	if maxPods >= 0 && t > maxPods {
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

// podCapacityUnknown is the maxPods sentinel computeMaxPods returns when node
// capacity cannot be determined. It disables the capacity guard rather than
// clamping the pool to zero.
const podCapacityUnknown int64 = -1

// computeMaxPods returns the maximum number of sandbox pods the cluster can
// actually host: what is left of the nodes' allocatable memory and CPU once
// every non-sandbox pod's requests are subtracted, with a 10% buffer, divided
// by the per-sandbox footprint, bounded by the tighter of the two resources.
//
// Subtracting the rest of the cluster is the whole point: system workloads
// (cert-manager, CoreDNS, metrics-server, ...) hold their requests for their
// entire lifetime, so sizing the pool against raw node allocatable overcounts
// and makes the reconciler create pods the scheduler can never place.
//
// Returns podCapacityUnknown when node metrics are unavailable (no limit
// enforced).
func computeMaxPods(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig) int64 {
	nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return podCapacityUnknown
	}
	totalMem, totalCPU := sumNodeAllocatable(nodes.Items)
	if totalMem == 0 && totalCPU == 0 {
		return podCapacityUnknown
	}
	usedMem, usedCPU := foreignPodRequests(ctx, k8s, cfg.Namespace)
	perPodMem, perPodCPU := perPodResources(cfg)

	var bounds []int64
	if totalMem > 0 {
		bounds = append(bounds, capacityFor(totalMem-usedMem, perPodMem.Value()))
	}
	if totalCPU > 0 {
		bounds = append(bounds, capacityFor(totalCPU-usedCPU, perPodCPU.MilliValue()))
	}
	if len(bounds) == 0 {
		return podCapacityUnknown
	}
	// Both bounds are real numbers here (possibly 0 = "no room"), so a plain
	// minimum is right; tighterCapacity would treat a 0 as "unknown" and
	// hand back the looser bound.
	tightest := bounds[0]
	for _, b := range bounds[1:] {
		if b < tightest {
			tightest = b
		}
	}
	return tightest
}

// foreignPodRequests sums the memory (bytes) and CPU (millicores) requested by
// every pod that is not a sandbox pod. Sandbox pods are excluded because they
// are accounted for separately, by counting them against maxPods.
func foreignPodRequests(ctx context.Context, k8s kubernetes.Interface, sandboxNamespace string) (mem, cpu int64) {
	pods, err := k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podHoldsReservation(pod, sandboxNamespace) {
			continue
		}
		reqMem, reqCPU := podRequests(pod)
		mem += reqMem
		cpu += reqCPU
	}
	return mem, cpu
}

// podHoldsReservation reports whether a pod still occupies scheduler capacity.
// Sandbox pods are excluded because they are accounted for separately, by
// counting them against maxPods, and so are pods that already ran to
// completion or failed outright.
func podHoldsReservation(pod *corev1.Pod, sandboxNamespace string) bool {
	if pod.Namespace == sandboxNamespace {
		if _, managed := pod.Labels[sandboxgrpc.LabelState]; managed {
			return false
		}
	}
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// podRequests sums the memory (bytes) and CPU (millicores) a pod's containers
// ask the scheduler to reserve for them.
func podRequests(pod *corev1.Pod) (mem, cpu int64) {
	for _, c := range pod.Spec.Containers {
		if m, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem += m.Value()
		}
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu += q.MilliValue()
		}
	}
	return mem, cpu
}

// capacityCacheTTL is how long a computed capacity is reused before the
// cluster is measured again. Reconciling runs every 10s and measuring means
// listing every pod in the cluster, which is wasted API load at that rate.
const capacityCacheTTL = 30 * time.Second

// capacityCache memoizes computeMaxPods. A nil *capacityCache disables caching,
// which keeps the reconciler usable (and testable) without one.
type capacityCache struct {
	mu        sync.Mutex
	expiresAt time.Time
	maxPods   int64
}

func (c *capacityCache) get(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig) int64 {
	if c == nil {
		return computeMaxPods(ctx, k8s, cfg)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expiresAt) {
		return c.maxPods
	}
	c.maxPods = computeMaxPods(ctx, k8s, cfg)
	c.expiresAt = time.Now().Add(capacityCacheTTL)
	return c.maxPods
}

// capacityFor computes how many sandbox pods fit within an allocatable total
// with a 10% buffer. Returns 0 when the total or per-pod divisor is missing.

// sumNodeAllocatable sums allocatable memory (bytes) and CPU (millicores)
// across nodes.
func sumNodeAllocatable(nodes []corev1.Node) (mem, cpu int64) {
	for i := range nodes {
		if m := nodes[i].Status.Allocatable.Memory(); m != nil {
			mem += m.Value()
		}
		if c := nodes[i].Status.Allocatable.Cpu(); c != nil {
			cpu += c.MilliValue()
		}
	}
	return
}

// perPodResources returns the per-sandbox resource limits from config,
// falling back to the documented defaults when unset.
func perPodResources(cfg config.SandboxConfig) (mem, cpu resource.Quantity) {
	mem = resource.MustParse(cfg.DefaultMemory)
	if mem.IsZero() {
		mem = resource.MustParse("512Mi")
	}
	cpu = resource.MustParse(cfg.DefaultCPU)
	if cpu.IsZero() {
		cpu = resource.MustParse("500m")
	}
	return
}

// capacityFor computes how many sandbox pods fit within an allocatable total
// with a 10% buffer. Returns 0 when the total or per-pod divisor is missing.
func capacityFor(total, divisor int64) int64 {
	if total <= 0 || divisor <= 0 {
		return 0
	}
	return total * 9 / 10 / divisor
}

func updateSandboxMatrixStatus(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig, orch *sandboxgrpc.Orchestrator, maxPods int64) {
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
	// podCapacityUnknown (-1) is an internal sentinel; the status surface
	// reports it as 0, which is also what "no room left" looks like.
	reportedMax := maxPods
	if reportedMax < 0 {
		reportedMax = 0
	}
	status["maxPods"] = reportedMax
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
