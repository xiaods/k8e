package version

import (
	"fmt"
	"strings"

	"github.com/morikuni/aec"
	"github.com/spf13/cobra"
)

var (
	Program      = "k8e"
	ProgramUpper = strings.ToUpper(Program)
	Version      = "dev"
	GitCommit    = "HEAD"
)

func PrintK8eASCIIArt() {
	k8eLogo := aec.BlueF.Apply(k8eFigletStr)
	fmt.Print(k8eLogo)
}

func MakeVersion() *cobra.Command {
	var command = &cobra.Command{
		Use:          "version",
		Short:        "Print the version",
		Example:      `  k8e version`,
		SilenceUsage: false,
	}
	command.Run = func(cmd *cobra.Command, args []string) {
		PrintK8eASCIIArt()
		if len(Version) == 0 {
			fmt.Println("Version: dev")
		} else {
			fmt.Println("Version:", Version)
		}
		fmt.Println("Git Commit:", GitCommit)
	}
	return command
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
