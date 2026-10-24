package version

import (
	"fmt"
	"strings"

	"github.com/morikuni/aec"
)

var (
	Program      = "k8e"
	ProgramUpper = strings.ToUpper(Program)
	Version      = "dev"
	GitCommit    = "HEAD"

	UpstreamGolang = ""

	// CiliumChartVersion is the Cilium Helm chart shipped by k8e.
	// 1.20.x is e2e-tested against Kubernetes 1.33–1.36; Cilium freezes that
	// matrix for the life of the minor. The first line that lists Kubernetes
	// 1.37 is 1.21 (v1.21.0-pre.2 as of 2026-09-09, still pre-release, and it
	// vendors k8s.io/* v0.37.0). Stay on 1.20 until 1.21.0 is GA.
	// Gateway API CRDs in manifests/sandbox-matrix/gateway-api-crds.yaml are
	// pinned to v1.6.1 for this chart (standard + experimental TLSRoute v1alpha2).
	//
	// Do not go below 1.20.2: kernel v7.2 added validation that bpf_set_retval's
	// R1 must be a scalar (torvalds/linux b1f7f67b74c2, v7.2-rc1). Cilium's helper
	// probe emitted a bare call with R1 still holding the context pointer, so
	// 1.20.0/1.20.1 die at startup with "failed to probe helper ... R1 is not a
	// scalar" on any 7.2 kernel — including OrbStack. Fixed by 67c619cb (CO-RE
	// bpf_core_enum_value_exists), backported to v1.20 in PR #48213 and released
	// in 1.20.2. See cilium/cilium#48016.
	CiliumChartVersion = "1.20.2"
)

func PrintK8eASCIIArt() {
	k8eLogo := aec.BlueF.Apply(k8eFigletStr)
	fmt.Print(k8eLogo)
}

const k8eFigletStr = `
/$$        /$$$$$$           
| $$       /$$__  $$          
| $$   /$$| $$  \ $$  /$$$$$$ 
| $$  /$$/|  $$$$$$/ /$$__  $$
| $$$$$$/  >$$__  $$| $$$$$$$$
| $$_  $$ | $$  \ $$| $$_____/
| $$ \  $$|  $$$$$$/|  $$$$$$$
|__/  \__/ \______/  \_______/
                              
K8E - Isolated sandboxes for untrusted agent code, sandbox-ready Kubernetes.
`
