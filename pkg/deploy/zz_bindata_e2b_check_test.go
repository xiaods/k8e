package deploy

import (
	"bytes"
	"testing"
)

// TestE2BGatewayAPIManifestsStaged verifies the e2b ingress manifest bundle
// is complete and staged with the KIP-21 loopback-safe advertise-IP bridge.
// Kept as a thin composition of small helpers to stay under DeepSource's
// cognitive-complexity bound (GO-R1005).
func TestE2BGatewayAPIManifestsStaged(t *testing.T) {
	for _, name := range []string{
		"sandbox-matrix/e2b-gateway.yaml",
		"sandbox-matrix/gateway-api-crds.yaml",
	} {
		assetNonEmpty(t, name)
	}

	cilium := assetBytes(t, "cilium.yaml")
	assertContains(t, cilium, "gatewayAPI:", "cilium.yaml")
	assertContains(t, cilium, "enabled: true", "cilium.yaml")

	// The e2b ingress manifest must bridge BOTH host-resident services
	// (e2b HTTP :3676, sandbox gRPC :50051) into the cluster via headless
	// Service + discovery.k8s.io/v1 EndpointSlice — sandbox-matrix and
	// e2b-server both embed in the k8e-server host process (final KIP-18
	// architecture); no e2b Deployment, no legacy core v1 Endpoints.
	gw := assetBytes(t, "sandbox-matrix/e2b-gateway.yaml")
	assertAllContain(t, gw, "e2b-gateway.yaml", []string{
		"sandbox-grpc-gateway",
		"gateway.networking.k8s.io/v1",
		"io.cilium/gateway-controller",
		"discovery.k8s.io/v1",
		"kind: TCPRoute",
		"sectionName: grpc",
		"port: 50051",
		"port: 3676",
	})
	assertNoneContain(t, gw, "e2b-gateway.yaml", []string{"kind: Deployment", "kind: Endpoints"})
	for _, ep := range []string{"e2b-server", "sandbox-grpc-gateway"} {
		assertContains(t, gw, "kubernetes.io/service-name: "+ep, "e2b-gateway.yaml")
	}
	assertCount(t, gw, "kind: EndpointSlice", 2, "e2b-gateway.yaml")
	assertCount(t, gw, "addressType: IPv4", 2, "e2b-gateway.yaml")

	// KIP-21: the EndpointSlice addresses must come from the %{ADVERTISE_IP}%
	// template (resolved at stage time to a routable host address), and the
	// manifest must never contain a literal loopback — Kubernetes does not
	// allow loopback endpoint addresses (unreachable from pods).
	//
	// KIP-24 one-click expose: a third %{ADVERTISE_IP}% occurrence pins the
	// Gateway's LoadBalancer address (spec.addresses, type IPAddress) to the
	// same host private IP, and a fourth scopes the CiliumLoadBalancerIPPool
	// (/32) that LB-IPAM requires before it will actually assign that IP —
	// no MetalLB, no <pending> external-IP.
	assertCount(t, gw, "%{ADVERTISE_IP}%", 4, "e2b-gateway.yaml")
	assertContains(t, gw, "type: IPAddress", "e2b-gateway.yaml")
	assertContains(t, gw, "kind: CiliumLoadBalancerIPPool", "e2b-gateway.yaml")
	assertNoneContain(t, gw, "e2b-gateway.yaml", []string{"127.0.0.1", "::1"})

	// The CRD bundle must include TCPRoute (L4 passthrough listener).
	crds := assetBytes(t, "sandbox-matrix/gateway-api-crds.yaml")
	assertContains(t, crds, "tcproutes.gateway.networking.k8s.io", "gateway-api-crds.yaml")
}

func assetNonEmpty(t *testing.T, name string) {
	t.Helper()
	if len(assetBytes(t, name)) == 0 {
		t.Fatalf("Asset(%s) empty", name)
	}
}

func assetBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := Asset(name)
	if err != nil {
		t.Fatalf("Asset(%s): %v", name, err)
	}
	return b
}

func assertContains(t *testing.T, content []byte, want, name string) {
	t.Helper()
	if !bytes.Contains(content, []byte(want)) {
		t.Fatalf("%s missing %q", name, want)
	}
}

func assertAllContain(t *testing.T, content []byte, name string, wants []string) {
	t.Helper()
	for _, want := range wants {
		assertContains(t, content, want, name)
	}
}

func assertNoneContain(t *testing.T, content []byte, name string, banned []string) {
	t.Helper()
	for _, b := range banned {
		if bytes.Contains(content, []byte(b)) {
			t.Fatalf("%s must not contain %q", name, b)
		}
	}
}

func assertCount(t *testing.T, content []byte, token string, want int, name string) {
	t.Helper()
	if n := bytes.Count(content, []byte(token)); n != want {
		t.Fatalf("%s must contain %q exactly %d times, got %d", name, token, want, n)
	}
}

// TestE2BPoolShape guards the pool manifest shapes: blocks[].cidr is a FLAT
// CIDR string and serviceSelector is a bare LabelSelector in cilium.io/v2
// (nested {cidr:…} / {selector:…} forms are rejected by the apiserver).
func TestE2BPoolShape(t *testing.T) {
	gw := assetBytes(t, "sandbox-matrix/e2b-gateway.yaml")
	assertContains(t, gw, "  blocks:\n  - cidr:", "e2b-gateway.yaml")
	assertContains(t, gw, "serviceSelector:\n    matchLabels:", "e2b-gateway.yaml")
	assertNoneContain(t, gw, "e2b-gateway.yaml", []string{
		"- cidr:\n      cidr:",
		"serviceSelector:\n    selector:",
	})
}

// TestE2BEndpointSlicesBothParsed guards against silently-dropped YAML
// documents (a missing --- separator merged the e2b-server EndpointSlice into
// the pool doc, which made the parser emit ONE EndpointSlice — Gateway→e2b
// 'no healthy upstream' on every fresh install).
func TestE2BEndpointSlicesBothParsed(t *testing.T) {
	content := assetBytes(t, "sandbox-matrix/e2b-gateway.yaml")
	if got := bytes.Count(content, []byte("kind: EndpointSlice")); got != 2 {
		t.Fatalf("expected 2 EndpointSlice docs in manifest, got %d", got)
	}
}
