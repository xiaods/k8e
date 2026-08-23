package grpc

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// TestExposeBaseURLFallback verifies the KIP-24 one-click expose URL chain:
// --sandbox-expose-base-url override → --sandbox-advertise-hostname (KIP-22)
// → the resolved host private IP (the same address pinned as the Cilium
// Gateway's LoadBalancer IP) → localhost. With no flags at all, the private
// -IP fallback must win over the legacy localhost default so `k8e sandbox
// expose` returns a working URL on a default single-host install.
func TestExposeBaseURLFallback(t *testing.T) {
	cases := []struct {
		name              string
		exposeBaseURL     string
		advertiseHostname string
		advertiseIP       string
		want              string
	}{
		{
			name:              "override wins",
			exposeBaseURL:     "http://gw.example.com/",
			advertiseHostname: "host.example.com",
			advertiseIP:       "10.0.0.5",
			want:              "http://gw.example.com",
		},
		{
			name:              "hostname second",
			advertiseHostname: "host.example.com",
			advertiseIP:       "10.0.0.5",
			want:              "http://host.example.com",
		},
		{
			name:        "private IP third (one-click default)",
			advertiseIP: "10.0.0.5",
			want:        "http://10.0.0.5",
		},
		{
			name: "localhost last resort",
			want: "http://localhost",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				exposeBaseURLOverride: tc.exposeBaseURL,
				advertiseHostname:     tc.advertiseHostname,
				advertiseIP:           tc.advertiseIP,
			}
			if got := s.exposeBaseURL(); got != tc.want {
				t.Fatalf("exposeBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewServerExposeURLBaseSeed checks NewServer seeds the orchestrator's
// restore-time URL base with the same fallback chain, so exposures restored
// after a gateway restart keep pointing at the Gateway LB address. A fake
// dynamic client is required: NewServer kicks off RestoreExposedRegistry in
// the background (explicit resource→listKind mapping to avoid fake client
// pluralisation bugs).
func TestNewServerExposeURLBaseSeed(t *testing.T) {
	scheme := runtime.NewScheme()
	sessionListGVK := schema.GroupVersionKind{Group: testGroupK8e, Version: "v1alpha1", Kind: "SandboxSessionList"}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: testGroupK8e, Version: "v1alpha1", Kind: "SandboxSession"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(sessionListGVK, &unstructured.UnstructuredList{})
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: testGroupK8e, Version: "v1alpha1", Resource: "sandboxsessions"}: "SandboxSessionList",
	})
	s := NewServer(ServerConfig{K8s: kubefake.NewSimpleClientset(), Dyn: dyn, AdvertiseIP: "192.168.1.10"})
	if got := s.orch.exposeURLBase; got != "http://192.168.1.10" {
		t.Fatalf("orch.exposeURLBase = %q, want http://192.168.1.10", got)
	}
}
