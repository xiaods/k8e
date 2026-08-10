package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SandboxMatrix configures the Agentic AI Sandbox Matrix for a namespace.
type SandboxMatrix struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxMatrixSpec   `json:"spec,omitempty"`
	Status SandboxMatrixStatus `json:"status,omitempty"`
}

type SandboxMatrixSpec struct {
	WarmPoolSize        int                 `json:"warmPoolSize,omitempty"`
	RuntimeClass        string              `json:"runtimeClass,omitempty"`
	SessionTTL          int                 `json:"sessionTTL,omitempty"`
	DefaultAllowedHosts []string            `json:"defaultAllowedHosts,omitempty"`
	ResourceLimits      corev1.ResourceList `json:"resourceLimits,omitempty"`
	RateLimits          *RateLimitSpec      `json:"rateLimits,omitempty"`
}

// RateLimitSpec configures per-tenant rate limiting.
type RateLimitSpec struct {
	// WriteBurst is the max burst size for mutating RPCs (CreateSession, Exec, etc.).
	WriteBurst int `json:"writeBurst,omitempty"`
	// WriteRate is the sustained rate per second for mutating RPCs.
	WriteRate float64 `json:"writeRate,omitempty"`
	// ReadBurst is the max burst size for read-only RPCs (ReadFile, ListFiles, etc.).
	ReadBurst int `json:"readBurst,omitempty"`
	// ReadRate is the sustained rate per second for read-only RPCs.
	ReadRate float64 `json:"readRate,omitempty"`
}

type SandboxMatrixStatus struct {
	ReadyWarmCount    int   `json:"readyWarmCount,omitempty"`
	ActiveSessions    int   `json:"activeSessions,omitempty"`
	ClaimedFromWarm   int64 `json:"claimedFromWarm,omitempty"`
	ColdStarts        int64 `json:"coldStarts,omitempty"`
	AvgClaimLatencyMs int64 `json:"avgClaimLatencyMs,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SandboxSession represents an active or warm sandbox session.
type SandboxSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSessionSpec   `json:"spec,omitempty"`
	Status SandboxSessionStatus `json:"status,omitempty"`
}

// SecretRef references a key in a same-namespace K8s Secret. Only the reference
// is stored on the CRD; values are resolved at exec time (#505 / KIP-12 B).
type SecretRef struct {
	SecretName string `json:"secretName,omitempty"`
	Key        string `json:"key,omitempty"`
	EnvVar     string `json:"envVar,omitempty"`
}

type SandboxSessionSpec struct {
	TenantID        string   `json:"tenantID,omitempty"`
	AllowedHosts    []string `json:"allowedHosts,omitempty"`
	RuntimeClass    string   `json:"runtimeClass,omitempty"`
	ParentSessionID string   `json:"parentSessionID,omitempty"`
	Depth           int      `json:"depth,omitempty"`
	// Env is a non-sensitive map of environment variables applied at exec time
	// (not baked into the pod spec) so warm-pool pods remain reusable.
	Env map[string]string `json:"env,omitempty"`
	// SecretRefs are resolved from K8s Secrets at exec time; values are never stored here.
	SecretRefs []SecretRef `json:"secretRefs,omitempty"`
}

type SandboxSessionStatus struct {
	Phase        SandboxPhase `json:"phase,omitempty"`
	PodName      string       `json:"podName,omitempty"`
	PodIP        string       `json:"podIP,omitempty"`
	WorkspacePVC string       `json:"workspacePVC,omitempty"`
	// WorkspaceScope is the session's isolated workspace subdirectory under
	// /workspace (M1 slice 2: sub-agents get `.sessions/<sid>` so their
	// workspace resets independently of the parent).
	WorkspaceScope string       `json:"workspaceScope,omitempty"`
	CreatedAt      *metav1.Time `json:"createdAt,omitempty"`
	ExpiresAt      *metav1.Time `json:"expiresAt,omitempty"`
}

type SandboxPhase string

const (
	SandboxPhaseWarm                SandboxPhase = "Warm"
	SandboxPhaseActive              SandboxPhase = "Active"
	SandboxPhaseResetting           SandboxPhase = "Resetting"
	SandboxPhaseTerminating         SandboxPhase = "Terminating"
	SandboxPhaseBackgroundRunning   SandboxPhase = "BackgroundRunning"
	SandboxPhaseBackgroundCompleted SandboxPhase = "BackgroundCompleted"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SandboxWarmPool manages a pool of pre-provisioned sandbox pods.
type SandboxWarmPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxWarmPoolSpec   `json:"spec,omitempty"`
	Status SandboxWarmPoolStatus `json:"status,omitempty"`
}

type SandboxWarmPoolSpec struct {
	TemplateRef  corev1.LocalObjectReference `json:"templateRef,omitempty"`
	Size         int                         `json:"size,omitempty"`
	RuntimeClass string                      `json:"runtimeClass,omitempty"`
	// MinSize is the adaptive lower bound for the pool target. Defaults to Size.
	// Only meaningful when MaxSize > Size (adaptive mode).
	MinSize int `json:"minSize,omitempty"`
	// MaxSize is the adaptive upper bound for the pool target. When set above
	// Size, the pool grows toward MaxSize on cold-start bursts (recent demand)
	// and decays back toward MinSize once demand subsides.
	MaxSize int `json:"maxSize,omitempty"`
	// IdleTTLSeconds overrides the warm-pod idle reaping TTL for pods created
	// from this pool. Defaults to SandboxMatrix.sessionTTL * 2 when unset.
	IdleTTLSeconds int `json:"idleTTLSeconds,omitempty"`
}

type SandboxWarmPoolStatus struct {
	ReadyCount   int `json:"readyCount,omitempty"`
	PendingCount int `json:"pendingCount,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SandboxTemplate defines the pod template used for sandbox sessions.
type SandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SandboxTemplateSpec `json:"spec,omitempty"`
}

type SandboxTemplateSpec struct {
	RuntimeClass   string              `json:"runtimeClass,omitempty"`
	AllowedHosts   []string            `json:"allowedHosts,omitempty"`
	ResourceLimits corev1.ResourceList `json:"resourceLimits,omitempty"`
	Image          string              `json:"image,omitempty"`
}

// Ensure resource package is used
var _ = resource.Quantity{}
