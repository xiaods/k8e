package deploy

import (
	"strings"
	"testing"
)

// The Cilium release is delivered as a Helm chart staged by k8e at runtime, so
// unlike cert-manager its images are not visible in the embedded manifests and
// must be pinned here. Cilium 1.20.0/1.20.1 cannot start on kernel v7.2 (see
// pkg/version/version.go), which is why the floor is 1.20.2.
//
// cilium-envoy is on its own independent version line — cilium-envoy:v1.20.2
// does not exist — and is pinned by the chart's values.yaml.
const (
	ciliumAgentVersion     = "v1.20.2"
	ciliumOperatorVersion  = "v1.20.2"
	ciliumEnvoyVersion     = "v1.37.6-1789133542-cbec91f666af0bf742da986d43832932dbb26b82"
	ciliumAgentRepository  = "quay.io/cilium/cilium"
	ciliumEnvoyRepository  = "quay.io/cilium/cilium-envoy"
	ciliumOperatorRepoVar  = "quay.io/cilium/operator-generic"
)

// ciliumAirgapImages lists every image a Cilium agent/operator pair needs.
func ciliumAirgapImages() []string {
	return []string{
		ciliumAgentRepository + ":" + ciliumAgentVersion,
		ciliumEnvoyRepository + ":" + ciliumEnvoyVersion,
		ciliumOperatorRepoVar + ":" + ciliumOperatorVersion,
	}
}

// TestCiliumAirgapCoverage fails when the offline archive would ship a cluster
// that cannot start its CNI.
func TestCiliumAirgapCoverage(t *testing.T) {
	packaged := airgapImageSet(t)
	for _, image := range ciliumAirgapImages() {
		if !packaged[image] {
			t.Errorf("cilium image missing from airgap list: %s", image)
		}
	}
}

// TestCiliumAirgapVersions guards the 1.20.2 floor. Dropping back to 1.20.0 or
// 1.20.1 reintroduces the "failed to probe helper ... R1 is not a scalar"
// fatal on kernel v7.2, and pulling a non-existent cilium-envoy tag breaks the
// chart, so both directions are asserted.
func TestCiliumAirgapVersions(t *testing.T) {
	for _, image := range ciliumAirgapImages() {
		switch {
		case strings.HasPrefix(image, ciliumEnvoyRepository+":"):
			if !strings.HasPrefix(image, ciliumEnvoyRepository+":"+ciliumEnvoyVersion) {
				t.Errorf("cilium-envoy pinned to an unexpected version: %s", image)
			}
		case strings.HasPrefix(image, ciliumAgentRepository+":"):
			if image != ciliumAgentRepository+":"+ciliumAgentVersion {
				t.Errorf("cilium agent pinned to an unexpected version: %s", image)
			}
		case strings.HasPrefix(image, ciliumOperatorRepoVar+":"):
			if image != ciliumOperatorRepoVar+":"+ciliumOperatorVersion {
				t.Errorf("cilium operator pinned to an unexpected version: %s", image)
			}
		}
	}

	for _, bad := range []string{
		ciliumAgentRepository + ":v1.20.0",
		ciliumAgentRepository + ":v1.20.1",
		ciliumOperatorRepoVar + ":v1.20.0",
		ciliumOperatorRepoVar + ":v1.20.1",
	} {
		if airgapImageSet(t)[bad] {
			t.Errorf("airgap list still pins %s, which cannot start on kernel v7.2", bad)
		}
	}
}
