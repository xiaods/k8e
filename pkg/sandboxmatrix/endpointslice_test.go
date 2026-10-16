package sandboxmatrix

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func workerNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func controlPlaneNode(name string, addresses ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
		Status: corev1.NodeStatus{Addresses: addresses},
	}
}

func etcdNode(name string, addresses ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"node-role.kubernetes.io/etcd": "true"},
		},
		Status: corev1.NodeStatus{Addresses: addresses},
	}
}

func TestServerAddressesCollectsServerNodesOnly(t *testing.T) {
	nodes := []*corev1.Node{
		controlPlaneNode("cp-1",
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
			corev1.NodeAddress{Type: corev1.NodeHostName, Address: "cp-1"}),
		etcdNode("etcd-2", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}),
		workerNode("worker"),
	}
	addresses := serverAddresses(derefNodes(nodes))
	if len(addresses) != 2 || addresses[0] != "10.0.0.1" || addresses[1] != "10.0.0.2" {
		t.Fatalf("server addresses = %v, want the two server nodes only", addresses)
	}
}

func TestServerAddressesSkipsLoopbackAndDeduplicates(t *testing.T) {
	nodes := []*corev1.Node{
		controlPlaneNode("cp-1",
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "127.0.0.1"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}),
		controlPlaneNode("cp-2", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}),
	}
	addresses := serverAddresses(derefNodes(nodes))
	if len(addresses) != 1 || addresses[0] != "10.0.0.1" {
		t.Fatalf("server addresses = %v, want [10.0.0.1]", addresses)
	}
}

func TestReconcileEndpointSliceCreatesThenUpdates(t *testing.T) {
	k8s := fake.NewSimpleClientset(
		controlPlaneNode("cp-1", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}),
	)
	cfg := defaultCfg()

	reconcileEndpointSlice(context.Background(), k8s, cfg)

	slice, err := k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Get(context.Background(), gatewayServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("slice not created: %v", err)
	}
	if len(slice.Endpoints) != 1 || slice.Endpoints[0].Addresses[0] != "10.0.0.1" {
		t.Fatalf("slice endpoints = %v", slice.Endpoints)
	}
	if slice.Labels[discoveryv1.LabelServiceName] != gatewayServiceName {
		t.Fatalf("slice missing service-name label: %v", slice.Labels)
	}

	// A second server joins: the slice grows on the next reconcile.
	cp2 := controlPlaneNode("cp-2", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.2"})
	if _, err := k8s.CoreV1().Nodes().Create(context.Background(), cp2, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileEndpointSlice(context.Background(), k8s, cfg)
	slice, err = k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Get(context.Background(), gatewayServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(slice.Endpoints) != 2 {
		t.Fatalf("slice endpoints = %v, want 2", slice.Endpoints)
	}
}

func TestReconcileEndpointSliceKeepsSliceWhenNoServerAddresses(t *testing.T) {
	k8s := fake.NewSimpleClientset(
		controlPlaneNode("cp-1", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}),
	)
	cfg := defaultCfg()
	reconcileEndpointSlice(context.Background(), k8s, cfg)

	// All server nodes lost their addresses (mid-registration): the existing
	// slice must survive rather than being emptied.
	node, err := k8s.CoreV1().Nodes().Get(context.Background(), "cp-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = nil
	if _, err := k8s.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileEndpointSlice(context.Background(), k8s, cfg)
	slice, err := k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Get(context.Background(), gatewayServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(slice.Endpoints) != 1 {
		t.Fatalf("slice was emptied: %v", slice.Endpoints)
	}
}

func TestSliceUpToDate(t *testing.T) {
	mk := func(addresses ...string) *discoveryv1.EndpointSlice {
		ready := true
		var endpoints []discoveryv1.Endpoint
		for _, address := range addresses {
			endpoints = append(endpoints, discoveryv1.Endpoint{
				Addresses:  []string{address},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			})
		}
		return &discoveryv1.EndpointSlice{Endpoints: endpoints}
	}
	if !sliceUpToDate(mk("10.0.0.1", "10.0.0.2"), []string{"10.0.0.2", "10.0.0.1"}) {
		t.Fatal("same membership must be up to date")
	}
	if sliceUpToDate(mk("10.0.0.1"), []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatal("missing address must require an update")
	}
	notReady := &discoveryv1.EndpointSlice{Endpoints: []discoveryv1.Endpoint{{
		Addresses:  []string{"10.0.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(false)},
	}}}
	if sliceUpToDate(notReady, []string{"10.0.0.1"}) {
		t.Fatal("not-ready endpoint must not count as present")
	}
}

func derefNodes(nodes []*corev1.Node) []corev1.Node {
	out := make([]corev1.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *n)
	}
	return out
}
