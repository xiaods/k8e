// KIP-30 M3.1: leader-gated sandbox-grpc-gateway EndpointSlice.
//
// manifests/sandbox-matrix/e2b-gateway.yaml stages a static
// discovery.k8s.io/v1 EndpointSlice whose single address is
// %{ADVERTISE_IP}% — the leader's own advertise IP, resolved at stage time
// on whichever node happened to run the deploy watcher first. That is correct
// for single-server installs and wrong for every HA one:
//
//   - A second server joining the cluster never appears in the slice, so the
//     gRPC gateway is only reachable through the first node's IP.
//   - When the first server is gone (crash, decommission, IP change), the
//     slice keeps pointing at a dead address; the endpoint-controller does
//     not fix it because a manually created slice has no controller behind it.
//   - Node IPs change (new network, EIP reassociation) and the staged slice
//     silently keeps the stale one until a restart re-stages.
//
// This controller moves the slice's membership into reconcile time, under the
// same sandboxmatrix-controller Lease as the warm-pool/GC loops: exactly one
// node writes it, from the live cluster state — every server node (control-
// plane or etcd labeled, the same set the HA control plane's dynamiclistener
// CN filter uses) with its current Internal/ExternalIP addresses. A node
// joining or leaving updates the slice within a reconcile tick; there is no
// last-writer-wins drift because only the lease holder writes.
//
// The staged manifest keeps Service/Gateway/TCPRoute/HTTPRoute objects but
// drops the static slice; this controller adopts the object with the
// sandbox.k8e.io/managed-by: k8e-sandboxmatrix label and its own field
// manager. K8E_SKIP_ENDPOINTSLICE_CONTROLLER is the documented escape hatch
// for operators who manage the object externally (MetalLB-style VIP fronting,
// cloud LB membership).
package sandboxmatrix

import (
	"context"
	"net"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/util"
)

const (
	// gatewayServiceName matches the headless Service in e2b-gateway.yaml that
	// the TCPRoute and in-cluster clients dial (sandbox-grpc-gateway.<ns>.svc).
	gatewayServiceName = "sandbox-grpc-gateway"
	// endpointSliceSyncInterval bounds how stale the slice may get between
	// external membership changes (node add/remove, IP change) that do not
	// otherwise wake this loop.
	endpointSliceSyncInterval = 30 * time.Second
	// endpointSlicePort is the gateway listen port (cfg.GRPCPort), carried on
	// every endpoint of the slice.
	endpointSlicePortName = "grpc"
)

// serverAddresses returns the routable addresses of every server node:
// Internal/ExternalIPs of control-plane or etcd labeled nodes, deduplicated
// and sorted. A node with no routable address yet (still registering) is
// skipped; an empty list means "nothing known" and the caller keeps the
// previous state rather than writing an empty slice.
func serverAddresses(nodes []corev1.Node) []string {
	seen := map[string]bool{}
	var addresses []string
	for i := range nodes {
		node := &nodes[i]
		if node.Labels[util.ControlPlaneRoleLabelKey] == "" && node.Labels[util.ETCDRoleLabelKey] == "" {
			continue
		}
		for _, address := range node.Status.Addresses {
			if address.Type != corev1.NodeInternalIP && address.Type != corev1.NodeExternalIP {
				continue
			}
			if !isRoutableEndpointIP(address.Address) {
				continue
			}
			if seen[address.Address] {
				continue
			}
			seen[address.Address] = true
			addresses = append(addresses, address.Address)
		}
	}
	sort.Strings(addresses)
	return addresses
}

// isRoutableEndpointIP mirrors the staging-time check (pkg/server
// advertiseIP): Kubernetes rejects loopback endpoint addresses and pods
// cannot reach unspecified/multicast ones. An IPv6-mapped IPv4 (::ffff:x) is
// accepted: the address is already routable and kube's endpoint validation
// normalizes it.
func isRoutableEndpointIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return !parsed.IsLoopback() && !parsed.IsUnspecified() && !parsed.IsMulticast() && !parsed.IsLinkLocalUnicast()
}

// runEndpointSliceLoop reconciles the gateway EndpointSlice until ctx is
// cancelled. Only the elected leader runs it (see controller.go's
// leader-gated block).
func runEndpointSliceLoop(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig) {
	ticker := time.NewTicker(endpointSliceSyncInterval)
	defer ticker.Stop()
	for {
		reconcileEndpointSlice(ctx, k8s, cfg)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileEndpointSlice performs one membership pass: read the server nodes,
// compute the desired slice, and write it (create/update/patch) when it
// differs from the current state. Exported for tests.
func reconcileEndpointSlice(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig) {
	nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Debugf("sandbox-endpointslice: list nodes: %v", err)
		return
	}
	addresses := serverAddresses(nodes.Items)
	if len(addresses) == 0 {
		// First boot before any server node reports an address; keep whatever
		// exists rather than writing an empty slice.
		logrus.Debugf("sandbox-endpointslice: no server node addresses yet")
		return
	}
	desired := desiredEndpointSlice(cfg, addresses)

	current, err := k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Get(ctx, gatewayServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			logrus.Warnf("sandbox-endpointslice: create: %v", err)
		}
		return
	}
	if err != nil {
		logrus.Debugf("sandbox-endpointslice: get: %v", err)
		return
	}
	if sliceUpToDate(current, addresses) {
		return
	}
	updateEndpointSlice(ctx, k8s, cfg, desired, current)
}

func desiredEndpointSlice(cfg config.SandboxConfig, addresses []string) *discoveryv1.EndpointSlice {
	endpoints := make([]discoveryv1.Endpoint, 0, len(addresses))
	for _, address := range addresses {
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses:  []string{address},
			Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)},
		})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayServiceName,
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName:   gatewayServiceName,
				"app.kubernetes.io/managed-by": "k8e-sandboxmatrix",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptrString(endpointSlicePortName),
			Port:     ptrInt32(int32(cfg.GRPCPort)),
			Protocol: ptrProtocol(corev1.ProtocolTCP),
		}},
	}
}

// sliceUpToDate reports whether the current slice already carries exactly the
// desired membership (address set equal, all ready). Port/label drift is
// corrected by the update path, not here.
func sliceUpToDate(current *discoveryv1.EndpointSlice, addresses []string) bool {
	currentSet := map[string]bool{}
	for _, endpoint := range current.Endpoints {
		if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
			continue
		}
		for _, address := range endpoint.Addresses {
			currentSet[address] = true
		}
	}
	desiredSet := map[string]bool{}
	for _, address := range addresses {
		desiredSet[address] = true
	}
	if len(currentSet) != len(desiredSet) {
		return false
	}
	for address := range desiredSet {
		if !currentSet[address] {
			return false
		}
	}
	return true
}

// updateEndpointSlice replaces the slice contents with a retry on conflict:
// the slice may also be touched by an operator kubectl edit or another k8e
// version's watcher, and losing the race must not fail the reconcile pass.
func updateEndpointSlice(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig, desired, current *discoveryv1.EndpointSlice) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, getErr := k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Get(ctx, gatewayServiceName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		latest.Labels = desired.Labels
		latest.AddressType = desired.AddressType
		latest.Endpoints = desired.Endpoints
		latest.Ports = desired.Ports
		_, updateErr := k8s.DiscoveryV1().EndpointSlices(cfg.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		logrus.Warnf("sandbox-endpointslice: update: %v", err)
		return
	}
	logrus.Infof("sandbox-endpointslice: %s now carries %d server address(es)",
		gatewayServiceName, len(desired.Endpoints))
	_ = current
}

func ptrBool(v bool) *bool       { return &v }
func ptrString(v string) *string { return &v }

func ptrInt32(v int32) *int32 { return &v }

func ptrProtocol(v corev1.Protocol) *corev1.Protocol { return &v }
