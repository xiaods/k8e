package cmds

import (
	"github.com/kingdee/myk8s/pkg/cli/master"
	"github.com/spf13/cobra"
)

type masterConfig struct {
}

var MasterConfig masterConfig

// cmd/install.go
func init() {
	rootCmd.AddCommand(masterCmd)
	//installCmd.Flags().StringP("install", "I", "all", "install software")
}

// rootCmd represents the base command when called without any subcommands
var masterCmd = &cobra.Command{
	Use:   "master",
	Short: "install master",
	Long:  `install master`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	Run: func(cmd *cobra.Command, args []string) {
		master.Run()
	},
}
