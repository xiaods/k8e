package cmds

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type MasterConfig struct {
	HTTPSPort            int
	APIServerBindAddress string
	DataDir              string
}

var Master MasterConfig

func NewMasterCommand(run func(cmd *cobra.Command, args []string)) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "master"
	cmd.Short = "Run management master"
	cmd.Long = "Run management master"
	cmd.Run = run
	cmd.Flags().IntVar(&Master.HTTPSPort, "https-listen-port", 6443, "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")
	viper.BindPFlag("https-listen-port", cmd.Flags().Lookup("https-listen-port"))
	return cmd
}

// var masterCmd = &cobra.Command{
// 	Use:   "master",
// 	Short: "Run management master",
// 	Long:  `install master`,
// 	// Uncomment the following line if your bare application
// 	// has an action associated with it:
// 	Run: func(cmd *cobra.Command, args []string) {
// 		master.Run()
// 	},
// }
