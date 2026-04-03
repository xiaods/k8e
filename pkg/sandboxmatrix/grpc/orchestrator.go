package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

const (
	maxDepth       = 1
	sandboxNS      = "sandbox-matrix"
	labelState     = "sandbox.k8e.io/state"
	labelSessionID = "sandbox.k8e.io/session-id"
	stateWarm      = "warm"
	stateActive    = "active"
	sandboxImage   = "ghcr.io/xiaods/k8e-sandbox:latest"
)

var (
	sessionGVR = schema.GroupVersionResource{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxsessions"}
	cnpGVR     = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	matrixGVR  = schema.GroupVersionResource{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxmatrices"}

	defaultAllowedHosts = []string{
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org",
		"github.com", "raw.githubusercontent.com", "objects.githubusercontent.com",
		"crates.io", "static.crates.io",
	}
)

type pendingApproval struct {
	action   string
	approved chan bool
}

// Orchestrator handles session lifecycle, sub-agent creation, and confirm_action gating.
type Orchestrator struct {
	k8s     kubernetes.Interface
	dynamic dynamic.Interface
	mu      sync.Mutex
	approvals map[string]*pendingApproval
}

func NewOrchestrator(k8s kubernetes.Interface, dyn dynamic.Interface) *Orchestrator {
	return &Orchestrator{k8s: k8s, dynamic: dyn, approvals: make(map[string]*pendingApproval)}
}

// defaultTTL is used when the session has no explicit TTL (0 = no expiry).
const defaultTTL = 0

func (o *Orchestrator) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*sandboxv1.SandboxSession, error) {
	matrixHosts, ttl, cpu, memory := o.getMatrixConfig(ctx)
	return o.createSessionWithTTL(ctx, req, ttl, matrixHosts, cpu, memory)
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
	session := &sandboxv1.SandboxSession{
		TypeMeta:   metav1.TypeMeta{APIVersion: "k8e.cattle.io/v1alpha1", Kind: "SandboxSession"},
		ObjectMeta: metav1.ObjectMeta{Name: sessionID, Namespace: sandboxNS},
		Spec: sandboxv1.SandboxSessionSpec{
			TenantID:     req.TenantId,
			AllowedHosts: allowedHosts,
			RuntimeClass: runtimeClass,
			Depth:        0,
		},
	}
	if err := o.createSession(ctx, session); err != nil {
		return nil, err
	}

	pvcName, err := o.ensureWorkspacePVC(ctx, sessionID)
	if err != nil {
		return nil, err
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

	o.deleteCNP(ctx, session)
	if session.Status.PodName != "" {
		o.k8s.CoreV1().Pods(sandboxNS).Delete(ctx, session.Status.PodName, metav1.DeleteOptions{}) //nolint:errcheck
	}
	if session.Status.WorkspacePVC != "" {
		o.k8s.CoreV1().PersistentVolumeClaims(sandboxNS).Delete(ctx, session.Status.WorkspacePVC, metav1.DeleteOptions{}) //nolint:errcheck
	}
	return o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Delete(ctx, sessionID, metav1.DeleteOptions{})
}

// ListActiveSessions returns all Active SandboxSessions in the given namespace.
func (o *Orchestrator) ListActiveSessions(ctx context.Context, namespace string) ([]*sandboxv1.SandboxSession, error) {
	list, err := o.dynamic.Resource(sessionGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var result []*sandboxv1.SandboxSession
	for i := range list.Items {
		s, err := unstructuredToSession(&list.Items[i])
		if err == nil && s.Status.Phase == sandboxv1.SandboxPhaseActive {
			result = append(result, s)
		}
	}
	return result, nil
}

func (o *Orchestrator) RunSubAgent(ctx context.Context, req *pb.RunSubAgentRequest) (*pb.RunSubAgentResponse, error) {
	parent, err := o.getSession(ctx, req.ParentSessionId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "parent session not found: %v", err)
	}
	if parent.Spec.Depth >= maxDepth {
		return nil, status.Errorf(codes.PermissionDenied, "max depth %d reached", maxDepth)
	}

	childID := fmt.Sprintf("%s-sub-%d", req.ParentSessionId, time.Now().UnixNano())
	child := &sandboxv1.SandboxSession{
		TypeMeta:   metav1.TypeMeta{APIVersion: "k8e.cattle.io/v1alpha1", Kind: "SandboxSession"},
		ObjectMeta: metav1.ObjectMeta{Name: childID, Namespace: sandboxNS},
		Spec: sandboxv1.SandboxSessionSpec{
			TenantID:        parent.Spec.TenantID,
			AllowedHosts:    parent.Spec.AllowedHosts,
			RuntimeClass:    parent.Spec.RuntimeClass,
			ParentSessionID: req.ParentSessionId,
			Depth:           parent.Spec.Depth + 1,
		},
	}
	if err := o.createSession(ctx, child); err != nil {
		return nil, status.Errorf(codes.Internal, "create sub-agent: %v", err)
	}

	// sub-agent shares parent's PVC (read-write) for filesystem-based IPC
	parentPVC := parent.Status.WorkspacePVC
	if parentPVC == "" {
		// parent may not have a PVC (e.g. warm pool pod) — create one
		var pvcErr error
		parentPVC, pvcErr = o.ensureWorkspacePVC(ctx, req.ParentSessionId)
		if pvcErr != nil {
			return nil, status.Errorf(codes.Internal, "parent PVC: %v", pvcErr)
		}
	}

	pod, err := o.claimOrCreatePod(ctx, childID, child.Spec.RuntimeClass, parentPVC, "", "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pod: %v", err)
	}

	child.Status.Phase = sandboxv1.SandboxPhaseActive
	child.Status.PodName = pod.Name
	child.Status.PodIP = pod.Status.PodIP
	child.Status.WorkspacePVC = parentPVC
	child.Status.CreatedAt = &metav1.Time{Time: time.Now()}
	o.updateSessionStatus(ctx, child)

	if err := o.applyCNP(ctx, child); err != nil {
		return nil, status.Errorf(codes.Internal, "network policy: %v", err)
	}
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
			return nil, status.Errorf(codes.Canceled, "cancelled")
		case <-time.After(30 * time.Second):
			o.mu.Lock()
			delete(o.approvals, req.ApprovalId)
			o.mu.Unlock()
			return nil, status.Errorf(codes.DeadlineExceeded, "timeout")
		}
	}

	approvalID := fmt.Sprintf("approval-%s-%d", req.SessionId, time.Now().UnixNano())
	o.mu.Lock()
	o.approvals[approvalID] = &pendingApproval{action: req.Action, approved: make(chan bool, 1)}
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
	pods, err := o.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
		LabelSelector: labelState + "=" + stateWarm,
	})
	if err == nil {
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			pod.Labels[labelState] = stateActive
			pod.Labels[labelSessionID] = sessionID
			updated, err := o.k8s.CoreV1().Pods(sandboxNS).Update(ctx, pod, metav1.UpdateOptions{})
			if err == nil {
				return updated, nil
			}
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sandbox-%s", sessionID),
			Namespace: sandboxNS,
			Labels:    map[string]string{labelState: stateActive, labelSessionID: sessionID},
		},
		Spec: sandboxPodSpec(runtimeClass, pvcName, cpu, memory),
	}
	return o.k8s.CoreV1().Pods(sandboxNS).Create(ctx, pod, metav1.CreateOptions{})
}

func sandboxPodSpec(runtimeClass, pvcName, cpu, memory string) corev1.PodSpec {
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
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "sandbox",
			Image: sandboxImage,
			Ports: []corev1.ContainerPort{{ContainerPort: 2024}},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(memory),
				},
			},
			SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: boolPtr(true)},
			VolumeMounts:    []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
		}},
		Volumes:       []corev1.Volume{vol},
		RestartPolicy: corev1.RestartPolicyNever,
	}
	if runtimeClass != "" {
		spec.RuntimeClassName = &runtimeClass
	}
	return spec
}

func boolPtr(b bool) *bool { return &b }

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
	hosts := session.Spec.AllowedHosts
	if len(hosts) == 0 {
		hosts = defaultAllowedHosts
	}
	fqdns := make([]interface{}, len(hosts))
	for i, h := range hosts {
		fqdns[i] = map[string]interface{}{"matchName": h}
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
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
			"egress": []interface{}{
				map[string]interface{}{
					"toEndpoints": []interface{}{
						map[string]interface{}{"matchLabels": map[string]interface{}{
							"k8s:io.kubernetes.pod.namespace": "kube-system",
							"k8s:k8s-app":                    "kube-dns",
						}},
					},
					"toPorts": []interface{}{
						map[string]interface{}{
							"ports": []interface{}{map[string]interface{}{"port": "53", "protocol": "ANY"}},
							"rules": map[string]interface{}{"dns": []interface{}{map[string]interface{}{"matchPattern": "*"}}},
						},
					},
				},
				map[string]interface{}{
					"toFQDNs": fqdns,
					"toPorts": []interface{}{
						map[string]interface{}{
							"ports": []interface{}{map[string]interface{}{"port": "443", "protocol": "TCP"}},
						},
					},
				},
			},
		},
	}}

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

func (o *Orchestrator) deleteCNP(ctx context.Context, session *sandboxv1.SandboxSession) {
	name := fmt.Sprintf("sandbox-session-%s", session.Name)
	o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func sessionToUnstructured(s *sandboxv1.SandboxSession) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

func unstructuredToSession(u *unstructured.Unstructured) (*sandboxv1.SandboxSession, error) {
	data, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	var s sandboxv1.SandboxSession
	return &s, json.Unmarshal(data, &s)
}
