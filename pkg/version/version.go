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
