package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type SandboxMatrixList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []SandboxMatrix `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type SandboxSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []SandboxSession `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type SandboxWarmPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []SandboxWarmPool `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []SandboxTemplate `json:"items"`
}

func (in *SandboxMatrix) DeepCopyObject() runtime.Object     { return in.DeepCopy() }
func (in *SandboxMatrixList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *SandboxMatrix) DeepCopy() *SandboxMatrix {
	if in == nil {
		return nil
	}
	out := new(SandboxMatrix)
	in.DeepCopyInto(out)
	return out
}
func (in *SandboxMatrix) DeepCopyInto(out *SandboxMatrix) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}
func (in *SandboxMatrixSpec) DeepCopyInto(out *SandboxMatrixSpec) {
	*out = *in
	if in.DefaultAllowedHosts != nil {
		out.DefaultAllowedHosts = append([]string{}, in.DefaultAllowedHosts...)
	}
	if in.ResourceLimits != nil {
		in.ResourceLimits.DeepCopyInto(&out.ResourceLimits)
	}
}
func (in *SandboxMatrixList) DeepCopy() *SandboxMatrixList {
	if in == nil {
		return nil
	}
	out := new(SandboxMatrixList)
	*out = *in
	if in.Items != nil {
		out.Items = make([]SandboxMatrix, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}

func (in *SandboxSession) DeepCopyObject() runtime.Object     { return in.DeepCopy() }
func (in *SandboxSessionList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *SandboxSession) DeepCopy() *SandboxSession {
	if in == nil {
		return nil
	}
	out := new(SandboxSession)
	in.DeepCopyInto(out)
	return out
}
func (in *SandboxSession) DeepCopyInto(out *SandboxSession) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}
func (in *SandboxSessionSpec) DeepCopyInto(out *SandboxSessionSpec) {
	*out = *in
	if in.AllowedHosts != nil {
		out.AllowedHosts = append([]string{}, in.AllowedHosts...)
	}
}
func (in *SandboxSessionStatus) DeepCopyInto(out *SandboxSessionStatus) {
	*out = *in
	if in.CreatedAt != nil {
		t := in.CreatedAt.DeepCopy()
		out.CreatedAt = t
	}
	if in.ExpiresAt != nil {
		t := in.ExpiresAt.DeepCopy()
		out.ExpiresAt = t
	}
}
func (in *SandboxSessionList) DeepCopy() *SandboxSessionList {
	if in == nil {
		return nil
	}
	out := new(SandboxSessionList)
	*out = *in
	if in.Items != nil {
		out.Items = make([]SandboxSession, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}

func (in *SandboxWarmPool) DeepCopyObject() runtime.Object     { return in.DeepCopy() }
func (in *SandboxWarmPoolList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *SandboxWarmPool) DeepCopy() *SandboxWarmPool {
	if in == nil {
		return nil
	}
	out := new(SandboxWarmPool)
	in.DeepCopyInto(out)
	return out
}
func (in *SandboxWarmPool) DeepCopyInto(out *SandboxWarmPool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}
func (in *SandboxWarmPoolList) DeepCopy() *SandboxWarmPoolList {
	if in == nil {
		return nil
	}
	out := new(SandboxWarmPoolList)
	*out = *in
	if in.Items != nil {
		out.Items = make([]SandboxWarmPool, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}

func (in *SandboxTemplate) DeepCopyObject() runtime.Object     { return in.DeepCopy() }
func (in *SandboxTemplateList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
func (in *SandboxTemplate) DeepCopy() *SandboxTemplate {
	if in == nil {
		return nil
	}
	out := new(SandboxTemplate)
	in.DeepCopyInto(out)
	return out
}
func (in *SandboxTemplate) DeepCopyInto(out *SandboxTemplate) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}
func (in *SandboxTemplateSpec) DeepCopyInto(out *SandboxTemplateSpec) {
	*out = *in
	if in.AllowedHosts != nil {
		out.AllowedHosts = append([]string{}, in.AllowedHosts...)
	}
	if in.ResourceLimits != nil {
		in.ResourceLimits.DeepCopyInto(&out.ResourceLimits)
	}
}
func (in *SandboxTemplateList) DeepCopy() *SandboxTemplateList {
	if in == nil {
		return nil
	}
	out := new(SandboxTemplateList)
	*out = *in
	if in.Items != nil {
		out.Items = make([]SandboxTemplate, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}
