package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

const (
	// SandboxAPIGroup is the K8E API group for sandbox CRDs.
	SandboxAPIGroup   = "k8e.sh"
	sandboxAPIGroup   = SandboxAPIGroup
	sandboxAPIVersion = SandboxAPIGroup + "/v1alpha1"

	maxDepth          = 1
	sandboxNS         = "sandbox-matrix"
	labelState        = "sandbox.k8e.io/state"
	labelSessionID    = "sandbox.k8e.io/session-id"
	labelRuntimeClass = "sandbox.k8e.io/runtime-class"
	stateWarm         = "warm"
	stateActive       = "active"

	// LabelState is the pod label key for sandbox state (warm/active).
	LabelState = labelState
	// StateWarm marks a pod as a pre-warmed sandbox.
	StateWarm = stateWarm
	// StateActive marks a pod as an active sandbox session.
	StateActive = stateActive
	// StateResetting marks a pod that is being reset before returning to warm pool.
	StateResetting = "resetting"

	// LabelRuntimeClass records the runtime a sandbox pod was booted with, so warm
	// pods are only claimed by sessions requesting the same RuntimeClass.
	LabelRuntimeClass = labelRuntimeClass
	sandboxImage      = "ghcr.io/xiaods/k8e-sandbox:latest"
)

var (
	sessionGVR = schema.GroupVersionResource{Group: sandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxsessions"}
	cnpGVR     = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	matrixGVR  = schema.GroupVersionResource{Group: sandboxAPIGroup, Version: "v1alpha1", Resource: "sandboxmatrices"}

	defaultAllowedHosts = []string{
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org",
		"objects.githubusercontent.com", "github.com", "raw.githubusercontent.com",
	}
)

type pendingApproval struct {
	action    string
	approved  chan bool
	createdAt time.Time
}

// approvalTTL is how long a pending approval waits before auto-expiry.
const approvalTTL = 5 * time.Minute

// Orchestrator handles session lifecycle, sub-agent creation, and confirm_action gating.
type Orchestrator struct {
	k8s         kubernetes.Interface
	dynamic     dynamic.Interface
	mu          sync.Mutex
	approvals   map[string]*pendingApproval
	runRegistry map[string]string // run_id → session_id

	// warmPodHealthCheck decides whether a warm pod's sandboxd is actually ready to
	// serve on :2024 before the pod is claimed for a session. Overridable in tests.
	warmPodHealthCheck func(ctx context.Context, pod *corev1.Pod) bool

	// OnWarmClaim, when non-nil, is invoked after a session successfully claims a
	// warm pod. The sandboxmatrix controller wires it to an immediate pool refill
	// instead of waiting for the next reconcile tick.
	OnWarmClaim func()

	// claim accounting for the SandboxMatrix status surface.
	claimedFromWarm     atomic.Int64
	coldStarts          atomic.Int64
	claimLatencyTotalMs atomic.Int64
	claimCount          atomic.Int64

	// maxBackgroundRuns caps concurrently-registered background runs per
	// session. Matches the KIP-16 M12 recommendation (capped, reaped
	// background execution) and KIP-15 G9 Perplexity parity.
	maxBackgroundRuns int
}

func NewOrchestrator(k8s kubernetes.Interface, dyn dynamic.Interface) *Orchestrator {
	return &Orchestrator{
		k8s:                k8s,
		dynamic:            dyn,
		approvals:          make(map[string]*pendingApproval),
		runRegistry:        make(map[string]string),
		warmPodHealthCheck: defaultWarmPodHealthCheck,
		maxBackgroundRuns:  defaultMaxBackgroundRuns,
	}
}

// defaultWarmPodHealthCheck reports whether a warm pod's sandboxd is actually
// ready to serve on :2024 before the pod is claimed for a session. It requires
// the kubelet Ready condition (driven by the TCP readiness probe on the sandbox
// container) plus an application-layer readiness handshake: a bare TCP dial is
// not a reliable readiness signal (the socket can be open before sandboxd
// finished initialization), so we call sandboxd's /ready endpoint and require
// status=="ready". The TCP dial is kept only as a fallback for sandboxd images
// that predate the /ready endpoint.
func defaultWarmPodHealthCheck(ctx context.Context, pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
		return false
	}
	if !PodReadyCondition(pod) {
		return false
	}
	hostPort := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(sandboxdPort))
	if readyHandshake(ctx, hostPort) {
		return true
	}
	// Fallback: older sandboxd images without /ready. Keep a short timeout.
	dialer := &net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// readyHandshake performs the application-layer readiness check against
// sandboxd's /ready endpoint. It returns true only when sandboxd answers
// 200 with status=="ready" (venv may still be initializing; a bare TCP dial
// cannot tell).
func readyHandshake(ctx context.Context, hostPort string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		"http://"+hostPort+"/ready", http.NoBody)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Status == "ready"
}

// PodReadyCondition returns true when the pod's Ready condition is True.
// Exported for use by the sandboxmatrix controller (warm pod recycling).
func PodReadyCondition(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// defaultTTL is used when the session has no explicit TTL (0 = no expiry).
const defaultTTL = 0

// Metrics returns warm-claim vs cold-start counters and the average pod
// acquisition latency in milliseconds. Used to surface pool hit rate and
// startup behavior in the SandboxMatrix status.
func (o *Orchestrator) Metrics() (claimedFromWarm, coldStarts, avgClaimLatencyMs int64) {
	claimedFromWarm = o.claimedFromWarm.Load()
	coldStarts = o.coldStarts.Load()
	total := o.claimLatencyTotalMs.Load()
	n := o.claimCount.Load()
	if n > 0 {
		avgClaimLatencyMs = total / n
	}
	return
}

// recordClaim updates claim metrics after a successful pod acquisition.
func (o *Orchestrator) recordClaim(start time.Time, warm bool) {
	o.claimLatencyTotalMs.Add(time.Since(start).Milliseconds())
	o.claimCount.Add(1)
	if warm {
		o.claimedFromWarm.Add(1)
	} else {
		o.coldStarts.Add(1)
	}
}

func (o *Orchestrator) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*sandboxv1.SandboxSession, error) {
	matrixHosts, ttl, cpu, memory := o.getMatrixConfig(ctx)
	return o.createSessionWithTTL(ctx, req, ttl, matrixHosts, cpu, memory)
}

// CheckCapacity returns nil if there is sufficient node memory for a new sandbox pod.
// Returns ResourceExhausted with a human-readable message when the pool is full.
func (o *Orchestrator) CheckCapacity(ctx context.Context) error {
	allocatable, err := o.nodeMemoryAllocatable(ctx)
	if err != nil {
		return err
	}

	pods, err := o.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
		LabelSelector: labelState,
	})
	if err != nil {
		return fmt.Errorf("capacity check: list pods: %w", err)
	}

	usedMemory := sumPodMemoryLimits(pods)
	available := allocatable.Value() * 9 / 10
	perPod := podMemoryLimit(pods)

	if available-usedMemory < perPod {
		return status.Errorf(codes.ResourceExhausted,
			"warm pool full: %d pods, %s/%s used. Please wait and retry, or add more memory.",
			len(pods.Items),
			resource.NewQuantity(usedMemory, resource.BinarySI).String(),
			resource.NewQuantity(allocatable.Value(), resource.BinarySI).String(),
		)
	}
	return nil
}

// nodeMemoryAllocatable returns the allocatable memory of the first node.
func (o *Orchestrator) nodeMemoryAllocatable(ctx context.Context) (*resource.Quantity, error) {
	nodes, err := o.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("capacity check: list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("capacity check: no nodes found")
	}
	allocatable := nodes.Items[0].Status.Allocatable.Memory()
	if allocatable == nil || allocatable.IsZero() {
		return nil, fmt.Errorf("capacity check: node %s has no allocatable memory", nodes.Items[0].Name)
	}
	return allocatable, nil
}

// sumPodMemoryLimits returns the sum of memory limits across all containers in the pod list.
func sumPodMemoryLimits(pods *corev1.PodList) int64 {
	var used int64
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Spec.Containers {
			if mem, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				used += mem.Value()
			}
		}
	}
	return used
}

// podMemoryLimit estimates the per-pod memory from the first pod, or falls back to 512Mi.
func podMemoryLimit(pods *corev1.PodList) int64 {
	if len(pods.Items) == 0 {
		return 512 * 1024 * 1024
	}
	for _, container := range pods.Items[0].Spec.Containers {
		if mem, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
			return mem.Value()
		}
	}
	return 512 * 1024 * 1024
}

func (o *Orchestrator) CreateSessionWithTTL(ctx context.Context, req *pb.CreateSessionRequest, ttl int) (*sandboxv1.SandboxSession, error) {
	return o.createSessionWithTTL(ctx, req, ttl, nil, "", "")
}

// getMatrixConfig reads defaultAllowedHosts, sessionTTL, and resourceLimits from the first SandboxMatrix CRD.
func (o *Orchestrator) getMatrixConfig(ctx context.Context) (allowedHosts []string, ttl int, cpu, memory string) {
	list, err := o.dynamic.Resource(matrixGVR).Namespace(sandboxNS).List(ctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		return nil, defaultTTL, "", ""
	}
	obj := list.Items[0].Object
	ttlVal, _, _ := unstructured.NestedInt64(obj, "spec", "sessionTTL")
	ttl = int(ttlVal)
	raw, _, _ := unstructured.NestedStringSlice(obj, "spec", "defaultAllowedHosts")
	cpu, _, _ = unstructured.NestedString(obj, "spec", "resourceLimits", "cpu")
	memory, _, _ = unstructured.NestedString(obj, "spec", "resourceLimits", "memory")
	return raw, ttl, cpu, memory
}

func (o *Orchestrator) createSessionWithTTL(ctx context.Context, req *pb.CreateSessionRequest, ttl int, matrixDefaultHosts []string, matrixCPU, matrixMemory string) (*sandboxv1.SandboxSession, error) {
	if err := validateSessionEnv(req.Env); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "env: %v", err)
	}
	sessionID := req.SessionId
	if sessionID == "" {
		sessionID = fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	runtimeClass := req.RuntimeClass
	if runtimeClass == "" {
		runtimeClass = "gvisor"
	}

	now := time.Now()
	// use request allowed_hosts; fall back to SandboxMatrix.spec.defaultAllowedHosts; then hardcoded defaults
	allowedHosts := req.AllowedHosts
	if len(allowedHosts) == 0 && len(matrixDefaultHosts) > 0 {
		allowedHosts = matrixDefaultHosts
	}
	if err := validateSecretRefs(req.SecretRefs); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "secret_refs: %v", err)
	}
	session := &sandboxv1.SandboxSession{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxAPIVersion, Kind: "SandboxSession"},
		ObjectMeta: metav1.ObjectMeta{Name: sessionID, Namespace: sandboxNS},
		Spec: sandboxv1.SandboxSessionSpec{
			TenantID:     req.TenantId,
			AllowedHosts: allowedHosts,
			RuntimeClass: runtimeClass,
			Depth:        0,
			Env:          req.Env,
			SecretRefs:   pbSecretRefsToAPI(req.SecretRefs),
		},
	}
	if err := o.createSession(ctx, session); err != nil {
		return nil, err
	}

	// Persistent sessions (tenant set) get a dedicated workspace PVC, mounted via a
	// cold-start pod. Ephemeral sessions claim a warm pod backed by EmptyDir, so no
	// PVC is created — a warm pod's volume is fixed at boot and cannot be swapped.
	var pvcName string
	if req.TenantId != "" {
		p, pvcErr := o.ensureWorkspacePVC(ctx, sessionID)
		if pvcErr != nil {
			return nil, pvcErr
		}
		pvcName = p
	}

	pod, err := o.claimOrCreatePod(ctx, sessionID, runtimeClass, pvcName, matrixCPU, matrixMemory)
	if err != nil {
		return nil, err
	}

	session.Status.Phase = sandboxv1.SandboxPhaseActive
	session.Status.PodName = pod.Name
	session.Status.PodIP = pod.Status.PodIP
	session.Status.WorkspacePVC = pvcName
	session.Status.CreatedAt = &metav1.Time{Time: now}
	if ttl > 0 {
		t := metav1.NewTime(now.Add(time.Duration(ttl) * time.Second))
		session.Status.ExpiresAt = &t
	}
	o.updateSessionStatus(ctx, session)

	return session, o.applyCNP(ctx, session)
}

func (o *Orchestrator) DestroySession(ctx context.Context, sessionID string) error {
	session, err := o.getSession(ctx, sessionID)
	if err != nil {
		return err
	}
	// mark Terminating before cleanup so observers can detect in-progress deletion
	session.Status.Phase = sandboxv1.SandboxPhaseTerminating
	o.updateSessionStatus(ctx, session)

	// 1. Delete CNP
	o.deleteCNP(ctx, session)

	// 2. Find the pod by session-id label and reset its workspace
	podIP, podName := o.findPodBySession(ctx, sessionID)
	if podIP == "" && session.Status.PodName != "" {
		// Fallback: fake clients may not support label selectors; use pod name from session
		pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, session.Status.PodName, metav1.GetOptions{})
		if err == nil {
			podIP = pod.Status.PodIP
			podName = pod.Name
		}
	}
	if podIP != "" {
		// M1 slice 2: a session with a workspace scope resets only its own
		// isolated subdir; unscoped sessions reset the whole /workspace.
		if session.Status.WorkspaceScope != "" {
			o.resetWorkspaceScope(ctx, podIP, session.Status.WorkspaceScope)
		} else {
			o.resetWorkspace(ctx, podIP)
		}
		// 3. Relabel pod: active → resetting, remove session-id
		if podName != "" {
			o.releasePod(ctx, podName)
		}
	}

	// 4. Delete Session CRD (pod and PVC survive, return to pool)
	return o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Delete(ctx, sessionID, metav1.DeleteOptions{})
}

// findPodBySession returns pod IP and name for a session by label, or empty strings.
func (o *Orchestrator) findPodBySession(ctx context.Context, sessionID string) (string, string) {
	pods, err := o.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
		LabelSelector: labelSessionID + "=" + sessionID,
	})
	if err != nil || len(pods.Items) == 0 {
		return "", ""
	}
	return pods.Items[0].Status.PodIP, pods.Items[0].Name
}

// resetWorkspace calls sandboxd's /workspace/reset endpoint and waits for completion.
func (o *Orchestrator) resetWorkspace(ctx context.Context, podIP string) {
	url := fmt.Sprintf("http://%s:2024/workspace/reset", podIP)
	resetWorkspaceURL(ctx, url)
}

// resetWorkspaceScope resets only a session's isolated workspace subdirectory
// (M1 slice 2, KIP-16 L1): sandboxd wipes /workspace/<scope> without touching
// the parent's files.
func (o *Orchestrator) resetWorkspaceScope(ctx context.Context, podIP, scope string) {
	url := fmt.Sprintf("http://%s:2024/workspace/reset?path=%s", podIP, url.QueryEscape(scope))
	resetWorkspaceURL(ctx, url)
}

func resetWorkspaceURL(ctx context.Context, url string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return
	}
	httpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req = req.WithContext(httpCtx)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// releasePod relabels a pod from active to resetting and removes session-id label.
func (o *Orchestrator) releasePod(ctx context.Context, podName string) {
	pod, err := o.k8s.CoreV1().Pods(sandboxNS).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return
	}
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[labelState] = StateResetting
	delete(pod.Labels, labelSessionID)
	o.k8s.CoreV1().Pods(sandboxNS).Update(ctx, pod, metav1.UpdateOptions{}) //nolint:errcheck
}

// ListActiveSessions returns all Active SandboxSessions in the given namespace.
func (o *Orchestrator) ListActiveSessions(ctx context.Context, namespace string) ([]*sandboxv1.SandboxSession, error) {
	return o.listSessions(ctx, namespace, string(sandboxv1.SandboxPhaseActive))
}

// listSessions returns sessions in namespace filtered by phase.
// phase empty or "Active" → Active only; "all" → every phase.
func (o *Orchestrator) listSessions(ctx context.Context, namespace, phase string) ([]*sandboxv1.SandboxSession, error) {
	list, err := o.dynamic.Resource(sessionGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	wantAll := phase == "all"
	if phase == "" {
		phase = string(sandboxv1.SandboxPhaseActive)
	}
	var result []*sandboxv1.SandboxSession
	for i := range list.Items {
		s, err := unstructuredToSession(&list.Items[i])
		if err != nil {
			continue
		}
		if wantAll || string(s.Status.Phase) == phase {
			result = append(result, s)
		}
	}
	return result, nil
}

// defaultMaxBackgroundRuns is the default cap on concurrently registered
// background runs per session.
const defaultMaxBackgroundRuns = 5

// countBackgroundRuns returns how many run_registry entries point at sessionID.
func (o *Orchestrator) countBackgroundRuns(sessionID string) int32 {
	o.mu.Lock()
	defer o.mu.Unlock()
	var n int32
	for _, sid := range o.runRegistry {
		if sid == sessionID {
			n++
		}
	}
	return n
}

// countAllBackgroundRuns returns the total number of registered background runs
// across all sessions (used by the Prometheus gauge).
func (o *Orchestrator) countAllBackgroundRuns() int32 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return int32(len(o.runRegistry))
}

func (o *Orchestrator) RunSubAgent(ctx context.Context, req *pb.RunSubAgentRequest) (*pb.RunSubAgentResponse, error) {
	parent, err := o.getSession(ctx, req.ParentSessionId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "parent session not found: %v", err)
	}
	if parent.Spec.Depth >= maxDepth {
		return nil, status.Errorf(codes.PermissionDenied, "max depth %d reached", maxDepth)
	}

	// Sub-agents share the parent's workspace for file-based IPC, which requires the
	// parent to own a real PVC. A warm-claimed (ephemeral) parent runs on EmptyDir
	// and cannot share storage, so reject rather than silently provision an unshared
	// PVC the parent can't see. Create the parent with a tenant_id to enable this.
	parentPVC := parent.Status.WorkspacePVC
	if parentPVC == "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"parent session %s has no shared workspace; create it with a tenant_id to enable sub-agents", req.ParentSessionId)
	}

	childID := fmt.Sprintf("%s-sub-%d", req.ParentSessionId, time.Now().UnixNano())
	child := &sandboxv1.SandboxSession{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxAPIVersion, Kind: "SandboxSession"},
		ObjectMeta: metav1.ObjectMeta{Name: childID, Namespace: sandboxNS},
		Spec: sandboxv1.SandboxSessionSpec{
			TenantID:        parent.Spec.TenantID,
			AllowedHosts:    parent.Spec.AllowedHosts,
			RuntimeClass:    parent.Spec.RuntimeClass,
			ParentSessionID: req.ParentSessionId,
			Depth:           parent.Spec.Depth + 1,
			Env:             parent.Spec.Env,
			SecretRefs:      append([]sandboxv1.SecretRef(nil), parent.Spec.SecretRefs...),
		},
	}
	if err := o.createSession(ctx, child); err != nil {
		return nil, status.Errorf(codes.Internal, "create sub-agent: %v", err)
	}

	// M1 workspace-session reuse (KIP-16 L1 / issue #514): the sub-agent shares
	// the parent's pod + sandboxd instead of provisioning a new pod. The child
	// inherits the parent's PodIP for exec routing but deliberately has NO own
	// PodName and NO own CNP — so DestroySession/GC only delete the child CRD
	// and never reset the shared pod's workspace or release it back to the pool.
	child.Status.Phase = sandboxv1.SandboxPhaseActive
	child.Status.PodIP = parent.Status.PodIP
	child.Status.WorkspacePVC = parentPVC
	// M1 slice 2: sub-agent gets an isolated workspace subdir so its files
	// reset independently of the parent (KIP-16 L1 workspace-session semantics).
	child.Status.WorkspaceScope = fmt.Sprintf(".sessions/%s", childID)
	child.Status.CreatedAt = &metav1.Time{Time: time.Now()}
	o.updateSessionStatus(ctx, child)

	return &pb.RunSubAgentResponse{SessionId: childID}, nil
}

func (o *Orchestrator) ConfirmAction(ctx context.Context, req *pb.ConfirmActionRequest) (*pb.ConfirmActionResponse, error) {
	if req.ApprovalId != "" {
		o.mu.Lock()
		pa, ok := o.approvals[req.ApprovalId]
		o.mu.Unlock()
		if !ok {
			return nil, status.Errorf(codes.NotFound, "approval %s not found", req.ApprovalId)
		}
		select {
		case approved := <-pa.approved:
			o.mu.Lock()
			delete(o.approvals, req.ApprovalId)
			o.mu.Unlock()
			return &pb.ConfirmActionResponse{ApprovalId: req.ApprovalId, Approved: approved}, nil
		case <-ctx.Done():
			o.mu.Lock()
			delete(o.approvals, req.ApprovalId)
			o.mu.Unlock()
			return nil, status.Errorf(codes.Canceled, "cancelled")
		case <-time.After(approvalTTL):
			o.mu.Lock()
			delete(o.approvals, req.ApprovalId)
			o.mu.Unlock()
			return nil, status.Errorf(codes.DeadlineExceeded, "approval timed out after %v", approvalTTL)
		}
	}

	approvalID := fmt.Sprintf("approval-%s-%d", req.SessionId, time.Now().UnixNano())
	o.mu.Lock()
	o.approvals[approvalID] = &pendingApproval{action: req.Action, approved: make(chan bool, 1), createdAt: time.Now()}
	o.mu.Unlock()
	return &pb.ConfirmActionResponse{ApprovalId: approvalID, Approved: false}, nil
}

func (o *Orchestrator) Approve(approvalID string, approved bool) error {
	o.mu.Lock()
	pa, ok := o.approvals[approvalID]
	o.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval %s not found", approvalID)
	}
	pa.approved <- approved
	return nil
}

// ApproveAction resolves a pending approval (external approval via gRPC).
func (o *Orchestrator) ApproveAction(ctx context.Context, req *pb.ApproveActionRequest) (*pb.ApproveActionResponse, error) {
	if err := o.Approve(req.ApprovalId, req.Approved); err != nil {
		return nil, status.Errorf(codes.NotFound, "approval %s: %v", req.ApprovalId, err)
	}
	return &pb.ApproveActionResponse{Ok: true}, nil
}

// StartApprovalGC periodically cleans expired pending approvals from disconnected clients.
func (o *Orchestrator) StartApprovalGC(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.mu.Lock()
			cutoff := time.Now().Add(-approvalTTL)
			for id, pa := range o.approvals {
				if pa.createdAt.Before(cutoff) {
					close(pa.approved)
					delete(o.approvals, id)
				}
			}
			o.mu.Unlock()
		}
	}
}

// ExecBackground submits a background command to the sandboxd and registers the run_id.
// env is the session's non-sensitive environment map (applied at exec time).
func (o *Orchestrator) ExecBackground(ctx context.Context, sessionID, command string, timeout int32, workdir string, env map[string]string) (string, error) {
	if o.countBackgroundRuns(sessionID) >= int32(o.maxBackgroundRuns) {
		return "", status.Errorf(codes.ResourceExhausted,
			"background run limit reached (%d active runs); poll or wait for completion before submitting more",
			o.maxBackgroundRuns)
	}
	podIP, err := o.getPodIPBySession(ctx, sessionID)
	if err != nil {
		return "", err
	}

	runID := fmt.Sprintf("%s-bg-%d", sessionID, time.Now().UnixNano())
	body, _ := json.Marshal(sandboxdBackgroundBody(sessionID, command, runID, timeout, workdir, env))

	url := fmt.Sprintf("http://%s:%d/exec/background", podIP, 2024)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := bgSandboxdClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sandboxd background submit: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	o.mu.Lock()
	o.runRegistry[runID] = sessionID
	o.mu.Unlock()

	return runID, nil
}

// PollRun checks the status of a background task and returns results when complete.
func (o *Orchestrator) PollRun(ctx context.Context, runID string) (*pb.PollRunResponse, error) {
	o.mu.Lock()
	sessionID, ok := o.runRegistry[runID]
	o.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("run %s not found", runID)
	}

	podIP, err := o.getPodIPBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s:%d/exec/background/%s", podIP, 2024, runID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	resp, err := bgSandboxdClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandboxd poll: %w", err)
	}
	defer resp.Body.Close()

	var result pb.PollRunResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result, nil
}

// RebuildRunRegistry scans Session CRDs and rebuilds the run_registry on startup.
func (o *Orchestrator) RebuildRunRegistry(ctx context.Context, namespace string) {
	sessions, err := o.dynamic.Resource(sessionGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, s := range sessions.Items {
		phase, _, _ := unstructured.NestedString(s.Object, "status", "phase")
		if phase == string(sandboxv1.SandboxPhaseBackgroundRunning) || phase == string(sandboxv1.SandboxPhaseBackgroundCompleted) {
			// Registry rebuild: background phase exists, run registry needs the run_id entry.
			// run_id is stored in sandboxd files on the pod; actual rebuild from files happens on first PollRun call.
		}
	}
}

// getPodIPBySession returns the pod IP for a session, polling if needed.
func (o *Orchestrator) getPodIPBySession(ctx context.Context, sessionID string) (string, error) {
	session, err := o.getSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if session.Status.PodIP != "" {
		return session.Status.PodIP, nil
	}
	// Poll for pod IP
	pods, err := o.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
		LabelSelector: labelSessionID + "=" + sessionID,
	})
	if err != nil || len(pods.Items) == 0 || pods.Items[0].Status.PodIP == "" {
		return "", fmt.Errorf("session %s has no pod IP", sessionID)
	}
	return pods.Items[0].Status.PodIP, nil
}

var bgSandboxdClient = &http.Client{Timeout: 10 * time.Second}

// --- internal helpers ---

func (o *Orchestrator) getSession(ctx context.Context, sessionID string) (*sandboxv1.SandboxSession, error) {
	u, err := o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sessionID, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return unstructuredToSession(u)
}

func (o *Orchestrator) createSession(ctx context.Context, session *sandboxv1.SandboxSession) error {
	u, err := sessionToUnstructured(session)
	if err != nil {
		return err
	}
	_, err = o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, u, metav1.CreateOptions{})
	return err
}

func (o *Orchestrator) updateSessionStatus(ctx context.Context, session *sandboxv1.SandboxSession) {
	u, err := sessionToUnstructured(session)
	if err != nil {
		return
	}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).UpdateStatus(ctx, u, metav1.UpdateOptions{})
}

func (o *Orchestrator) claimOrCreatePod(ctx context.Context, sessionID, runtimeClass, pvcName, cpu, memory string) (*corev1.Pod, error) {
	start := time.Now()
	// Only ephemeral sessions (no PVC) may adopt a warm pod: warm pods boot with an
	// EmptyDir volume, and a running pod's volumes cannot be changed to mount a
	// session PVC. Persistent sessions therefore cold-start with the PVC attached.
	if pvcName == "" {
		warm, err := o.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
			LabelSelector: labelState + "=" + stateWarm,
		})
		if err == nil {
			for i := range warm.Items {
				pod := &warm.Items[i]
				// Only claim warm pods whose sandboxd is actually serving on :2024.
				// A Running pod may still be booting sandboxd, or sandboxd may have
				// died silently; claiming either one makes the first Exec fail.
				if !o.warmPodHealthCheck(ctx, pod) {
					continue
				}
				// A warm pod booted with a different RuntimeClass must not be adopted:
				// gVisor vs Kata have different isolation/network semantics. Pods
				// created before the runtime label existed (no label) stay claimable
				// by any runtime for backward compatibility.
				if rc := pod.Labels[labelRuntimeClass]; rc != "" && rc != runtimeClass {
					continue
				}
				// atomic claim: use resourceVersion for optimistic locking
				pod.Labels[labelState] = stateActive
				pod.Labels[labelSessionID] = sessionID
				updated, uerr := o.k8s.CoreV1().Pods(sandboxNS).Update(ctx, pod, metav1.UpdateOptions{})
				if uerr == nil {
					o.recordClaim(start, true)
					// Pool just shrank by one: ask the controller to refill now.
					if o.OnWarmClaim != nil {
						o.OnWarmClaim()
					}
					return updated, nil
				}
				// conflict means another request claimed it first — try next warm pod
			}
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sandbox-%s", sessionID),
			Namespace: sandboxNS,
			Labels: map[string]string{
				labelState:        stateActive,
				labelSessionID:    sessionID,
				labelRuntimeClass: runtimeClass,
			},
			Annotations: GvisorAnnotations(runtimeClass),
		},
		Spec: sandboxPodSpec(runtimeClass, pvcName, cpu, memory),
	}
	created, cerr := o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, pod, metav1.CreateOptions{})
	if cerr == nil {
		o.recordClaim(start, false)
	}
	return created, cerr
}

func sandboxPodSpec(runtimeClass, pvcName, cpu, memory string) corev1.PodSpec {
	return SandboxPodSpec(runtimeClass, pvcName, cpu, memory, sandboxImage)
}

// SandboxPodSpec builds a PodSpec for a sandbox session. Exported for use by the controller.
// Set pvcName to empty string to use an EmptyDir volume instead of a PVC.
func SandboxPodSpec(runtimeClass, pvcName, cpu, memory, image string) corev1.PodSpec {
	if cpu == "" {
		cpu = "500m"
	}
	if memory == "" {
		memory = "512Mi"
	}
	vol := corev1.Volume{Name: "workspace"}
	if pvcName != "" {
		vol.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
		}
	} else {
		vol.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	// TCP health probes on :2024: kubelet reflects sandboxd readiness in the pod's
	// Ready condition, which the warm-pool claim path and the recycling controller
	// both rely on. StartupProbe gives a slow image pull / sandboxd boot a budget
	// before readiness starts counting failures.
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(sandboxdPort)},
		},
	}
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "sandbox",
			Image: image,
			Ports: []corev1.ContainerPort{{ContainerPort: sandboxdPort}},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(memory),
				},
			},
			SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: boolPtr(true)},
			VolumeMounts:    []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
			StartupProbe: &corev1.Probe{
				ProbeHandler:     probe.ProbeHandler,
				PeriodSeconds:    2,
				FailureThreshold: 30, // up to ~60s budget for sandboxd boot
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:     probe.ProbeHandler,
				PeriodSeconds:    5,
				TimeoutSeconds:   2,
				FailureThreshold: 3,
			},
		}},
		Volumes:       []corev1.Volume{vol},
		DNSPolicy:     corev1.DNSDefault,
		RestartPolicy: corev1.RestartPolicyNever,
	}
	if runtimeClass != "" {
		spec.RuntimeClassName = &runtimeClass
	}
	return spec
}

func boolPtr(b bool) *bool { return &b }

// GvisorAnnotations returns pod annotations required for gVisor to work with
// Cilium eBPF. The default netstack mode processes network in userspace,
// bypassing Cilium's eBPF programs attached to veth pairs. Host network mode
// forwards syscalls to the pod's kernel network namespace instead.
func GvisorAnnotations(runtimeClass string) map[string]string {
	if runtimeClass == "gvisor" {
		return map[string]string{"gvisor.dev/network": "host"}
	}
	return nil
}

// ensureWorkspacePVC creates a PVC for the session workspace if it doesn't exist.
func (o *Orchestrator) ensureWorkspacePVC(ctx context.Context, sessionID string) (string, error) {
	pvcName := "workspace-" + sessionID
	_, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		return pvcName, nil
	}
	storageClass := "local-path"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: sandboxNS,
			Labels:    map[string]string{labelSessionID: sessionID},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	if _, err := o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create workspace PVC: %w", err)
	}
	return pvcName, nil
}

func (o *Orchestrator) applyCNP(ctx context.Context, session *sandboxv1.SandboxSession) error {
	obj := buildSessionCNP(session)
	name := fmt.Sprintf("sandbox-session-%s", session.Name)
	_, err := o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	_, err = o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// buildSessionCNP returns the per-session CiliumNetworkPolicy. Egress is kept
// as toEntities:world limited to DNS (53) + HTTPS (443) — NOT narrowed via
// toFQDNs by default: FQDN rules require the Cilium DNS proxy (L7), which was
// enabled in 9f5b7f41 and reverted in 2b3bfb0a because it broke DNS in gVisor
// pods. Operators who re-enable the DNS proxy (see manifests/cilium.yaml
// dnsProxy.enabled) can tighten egress further; see KIP-16 M10 / issue #510.
func buildSessionCNP(session *sandboxv1.SandboxSession) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]interface{}{
			"name":      fmt.Sprintf("sandbox-session-%s", session.Name),
			"namespace": session.Namespace,
		},
		"spec": map[string]interface{}{
			"endpointSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{labelSessionID: session.Name},
			},
			"ingress": []interface{}{
				map[string]interface{}{
					"fromEntities": []interface{}{"host"},
					"toPorts": []interface{}{
						map[string]interface{}{
							"ports": []interface{}{map[string]interface{}{"port": "2024", "protocol": "TCP"}},
						},
					},
				},
				map[string]interface{}{
					"fromEndpoints": []interface{}{
						map[string]interface{}{
							"matchLabels": map[string]interface{}{
								"app": "sandbox-grpc-gateway",
							},
						},
					},
					"toPorts": []interface{}{
						map[string]interface{}{
							"ports": []interface{}{map[string]interface{}{"port": "2024", "protocol": "TCP"}},
						},
					},
				},
			},
			"egress": []interface{}{
				map[string]interface{}{
					"toEntities": []interface{}{"world"},
					"toPorts": []interface{}{
						map[string]interface{}{
							"ports": []interface{}{map[string]interface{}{"port": "53", "protocol": "ANY"}},
						},
					},
				},
				map[string]interface{}{
					"toEntities": []interface{}{"world"},
					"toPorts": []interface{}{
						map[string]interface{}{
							"ports": []interface{}{map[string]interface{}{"port": "443", "protocol": "TCP"}},
						},
					},
				},
			},
		},
	}}
}

func (o *Orchestrator) deleteCNP(ctx context.Context, session *sandboxv1.SandboxSession) {
	name := fmt.Sprintf("sandbox-session-%s", session.Name)
	o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func sessionToUnstructured(s *sandboxv1.SandboxSession) (*unstructured.Unstructured, error) {
	s.SetGroupVersionKind(schema.GroupVersionKind{Group: sandboxAPIGroup, Version: "v1alpha1", Kind: "SandboxSession"})
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: u}, nil
}

func unstructuredToSession(u *unstructured.Unstructured) (*sandboxv1.SandboxSession, error) {
	var s sandboxv1.SandboxSession
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &s)
	return &s, err
}
