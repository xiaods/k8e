package cmds

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/xiaods/k8e/pkg/version"
)

type MasterConfig struct {
	HTTPSPort            int
	APIServerBindAddress string
	DataDir              string
	ServerURL            string
	TLSSan               []string
}

var Master MasterConfig

func NewMasterCommand(run func(cmd *cobra.Command, args []string)) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "master"
	cmd.Short = "Run management master"
	cmd.Long = "Run management master"
	cmd.Run = run
	cmd.Flags().IntVar(&Master.HTTPSPort, "https-listen-port", 6443, "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")
	cmd.Flags().StringVarP(&Master.DataDir, "data-dir", "d", "", "(data) Folder to hold state default /var/lib/k8e/"+version.Program+" or ${HOME}/.k8e/"+version.Program+" if not root")
	cmd.Flags().StringVarP(&Master.ServerURL, "server", "s", "", "(experimental/cluster) Server to connect to, used to join a cluster")
	viper.BindPFlag("https-listen-port", cmd.Flags().Lookup("https-listen-port"))
	viper.BindPFlag("data-dir", cmd.Flags().Lookup("data-dir"))
	viper.BindPFlag("server", cmd.Flags().Lookup("server"))
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
