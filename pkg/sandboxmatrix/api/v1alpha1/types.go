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
	WarmPoolSize        int               `json:"warmPoolSize,omitempty"`
	RuntimeClass        string            `json:"runtimeClass,omitempty"`
	SessionTTL          int               `json:"sessionTTL,omitempty"`
	DefaultAllowedHosts []string          `json:"defaultAllowedHosts,omitempty"`
	ResourceLimits      corev1.ResourceList `json:"resourceLimits,omitempty"`
}

type SandboxMatrixStatus struct {
	ReadyWarmCount int `json:"readyWarmCount,omitempty"`
	ActiveSessions int `json:"activeSessions,omitempty"`
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

type SandboxSessionSpec struct {
	TenantID        string   `json:"tenantID,omitempty"`
	AllowedHosts    []string `json:"allowedHosts,omitempty"`
	RuntimeClass    string   `json:"runtimeClass,omitempty"`
	ParentSessionID string   `json:"parentSessionID,omitempty"`
	Depth           int      `json:"depth,omitempty"`
}

type SandboxSessionStatus struct {
	Phase        SandboxPhase  `json:"phase,omitempty"`
	PodName      string        `json:"podName,omitempty"`
	PodIP        string        `json:"podIP,omitempty"`
	WorkspacePVC string        `json:"workspacePVC,omitempty"`
	CreatedAt    *metav1.Time  `json:"createdAt,omitempty"`
	ExpiresAt    *metav1.Time  `json:"expiresAt,omitempty"`
}

type SandboxPhase string

const (
	SandboxPhaseWarm        SandboxPhase = "Warm"
	SandboxPhaseActive      SandboxPhase = "Active"
	SandboxPhaseTerminating SandboxPhase = "Terminating"
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
