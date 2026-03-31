package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "k8e.cattle.io"
const Version = "v1alpha1"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&SandboxMatrix{},
		&SandboxMatrixList{},
		&SandboxSession{},
		&SandboxSessionList{},
		&SandboxWarmPool{},
		&SandboxWarmPoolList{},
		&SandboxTemplate{},
		&SandboxTemplateList{},
	)
	return nil
}
