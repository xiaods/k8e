package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

func newTestOrchestrator() *Orchestrator {
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "k8e.cattle.io", Version: "v1alpha1", Kind: "SandboxSession"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "k8e.cattle.io", Version: "v1alpha1", Kind: "SandboxSessionList"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicyList"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
	}
	dyn := dynfake.NewSimpleDynamicClient(scheme)
	k8s := kubefake.NewSimpleClientset()
	return NewOrchestrator(k8s, dyn)
}

func TestCreateSession_GeneratesID(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Name == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestCreateSession_DefaultRuntime(t *testing.T) {
	o := newTestOrchestrator()
	sess, err := o.CreateSession(context.Background(), &pb.CreateSessionRequest{SessionId: "test-rt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Spec.RuntimeClass != "gvisor" {
		t.Fatalf("expected default runtime gvisor, got %s", sess.Spec.RuntimeClass)
	}
}

func TestRunSubAgent_MaxDepthEnforced(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8e.cattle.io/v1alpha1",
			"kind":       "SandboxSession",
			"metadata":   map[string]interface{}{"name": "parent-deep", "namespace": sandboxNS},
			"spec":       map[string]interface{}{"depth": int64(1), "runtimeClass": "gvisor"},
		},
	}
	o.dynamic.Resource(sessionGVR).Namespace(sandboxNS).Create(ctx, parent, metav1.CreateOptions{})

	_, err := o.RunSubAgent(ctx, &pb.RunSubAgentRequest{ParentSessionId: "parent-deep"})
	if err == nil {
		t.Fatal("expected PermissionDenied error")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
	}
}

func TestDestroySession_NotFound(t *testing.T) {
	o := newTestOrchestrator()
	err := o.DestroySession(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}
