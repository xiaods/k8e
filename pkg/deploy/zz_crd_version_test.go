package deploy

import (
	"bytes"
	"testing"
)

func TestGatewayAPICRDsV161(t *testing.T) {
	b, err := Asset("sandbox-matrix/gateway-api-crds.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"backendtlspolicies.gateway.networking.k8s.io",
		"bundle-version: v1.6.1",
		"tlsroutes.gateway.networking.k8s.io",
		"v1alpha2",
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Fatalf("gateway-api-crds.yaml missing %q", want)
		}
	}
}
