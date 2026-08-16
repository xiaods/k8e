package deploy

import (
	"bytes"
	"testing"
)

func TestE2BGatewayAPIManifestsStaged(t *testing.T) {
	for _, name := range []string{
		"sandbox-matrix/e2b-gateway.yaml",
		"sandbox-matrix/gateway-api-crds.yaml",
	} {
		b, err := Asset(name)
		if err != nil {
			t.Fatalf("Asset(%s): %v", name, err)
		}
		if len(b) == 0 {
			t.Fatalf("Asset(%s) empty", name)
		}
	}
	cilium, err := Asset("cilium.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cilium, []byte("gatewayAPI:")) {
		t.Fatalf("cilium.yaml missing gatewayAPI block:\n%s", cilium)
	}
	if !bytes.Contains(cilium, []byte("enabled: true")) {
		t.Fatalf("cilium.yaml gatewayAPI not enabled")
	}

	// The e2b ingress manifest must bridge BOTH host-resident services
	// (e2b HTTP :3676, sandbox gRPC :50051) into the cluster via headless
	// Service + Endpoints — sandbox-matrix and e2b-server both embed in the
	// k8e-server host process (final KIP-18 architecture); no e2b Deployment.
	gw, err := Asset("sandbox-matrix/e2b-gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sandbox-grpc-gateway",
		"gateway.networking.k8s.io/v1",
		"io.cilium/gateway-controller",
		"e2b-server",
		"kind: TCPRoute",
		"sectionName: grpc",
		"port: 50051",
		"port: 3676",
	} {
		if !bytes.Contains(gw, []byte(want)) {
			t.Fatalf("e2b-gateway.yaml missing %q", want)
		}
	}
	// Both host services must have Endpoints (headless bridge), and there
	// must be NO e2b-server Deployment (embedded in k8e-server instead).
	if bytes.Contains(gw, []byte("kind: Deployment")) {
		t.Fatalf("e2b-gateway.yaml must not contain a Deployment (e2b embeds in k8e-server)")
	}
	for _, ep := range []string{"e2b-server", "sandbox-grpc-gateway"} {
		if !bytes.Contains(gw, []byte("name: "+ep)) {
			t.Fatalf("e2b-gateway.yaml missing Endpoints for %q", ep)
		}
	}

	// KIP-21: the Endpoints address must come from the %{ADVERTISE_IP}%
	// template (resolved at stage time to a routable host address), and the
	// manifest must never contain a literal loopback — Kubernetes does not
	// allow Endpoints to use 127.0.0.1/::1 (unreachable from pods).
	if n := bytes.Count(gw, []byte("%{ADVERTISE_IP}%")); n != 2 {
		t.Fatalf("e2b-gateway.yaml must template %%%%{ADVERTISE_IP}%%%% into both Endpoints subsets, got %d occurrences", n)
	}
	for _, loopback := range []string{"127.0.0.1", "::1"} {
		if bytes.Contains(gw, []byte(loopback)) {
			t.Fatalf("e2b-gateway.yaml must not contain literal %q (KIP-21: Endpoints cannot use loopback)", loopback)
		}
	}

	// The CRD bundle must include TCPRoute (L4 passthrough listener).
	crds, err := Asset("sandbox-matrix/gateway-api-crds.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(crds, []byte("tcproutes.gateway.networking.k8s.io")) {
		t.Fatalf("gateway-api-crds.yaml missing TCPRoute CRD")
	}
}
