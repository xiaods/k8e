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

	// CiliumChartVersion is the Cilium Helm chart shipped by k8e. 1.20.0 moved
	// the Gateway API controller to Gateway API v1.6.1 CRDs (see
	// manifests/sandbox-matrix/gateway-api-crds.yaml, which must stay in sync —
	// v1.6.1 standard + experimental TLSRoute with v1alpha2).
	CiliumChartVersion = "1.20.0"
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
                              
Get Kubernetes cluster the easy way.
`
