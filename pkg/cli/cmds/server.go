package cmds

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/xiaods/k8e/pkg/version"
)

type ServerConfig struct {
	HTTPSPort            int
	APIServerBindAddress string
	DataDir              string
	ServerURL            string
	TLSSan               []string
	DisableAgent         bool
	ClusterCIDR          string
	DisableCCM           bool
}

var Server ServerConfig

func NewServerCommand(run func(cmd *cobra.Command, args []string)) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "Server"
	cmd.Short = "Run management Server"
	cmd.Long = "Run management Server"
	cmd.Run = run
	cmd.Flags().IntVar(&Server.HTTPSPort, "https-listen-port", 6443, "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")
	cmd.Flags().StringVarP(&Server.DataDir, "data-dir", "d", "", "(data) Folder to hold state default /var/lib/k8e/"+version.Program+" or ${HOME}/.k8e/"+version.Program+" if not root")
	cmd.Flags().StringVarP(&Server.ServerURL, "server", "s", "", "(experimental/cluster) Server to connect to, used to join a cluster")
	cmd.Flags().StringArrayVar(&Server.TLSSan, "tls-san", nil, "(listener) Add additional hostname or IP as a Subject Alternative Name in the TLS cert")
	cmd.Flags().BoolVar(&Server.DisableAgent, "disable-agent", true, "Do not run a local agent and register a local kubelet")
	cmd.Flags().StringVar(&Server.ClusterCIDR, "cluster-cidr", "10.42.0.0/16", "(networking) Network CIDR to use for pod IPs")
	cmd.Flags().BoolVar(&Server.DisableCCM, "disable-cloud-controller", true, "(components) Disable "+version.Program+" default cloud controller manager")

	viper.BindPFlag("https-listen-port", cmd.Flags().Lookup("https-listen-port"))
	viper.BindPFlag("data-dir", cmd.Flags().Lookup("data-dir"))
	viper.BindPFlag("server", cmd.Flags().Lookup("server"))
	viper.BindPFlag("tls-san", cmd.Flags().Lookup("tls-san"))
	viper.BindPFlag("disable-agent", cmd.Flags().Lookup("disable-agent"))
	viper.BindPFlag("cluster-cidr", cmd.Flags().Lookup("cluster-cidr"))
	viper.BindPFlag("disable-cloud-controller", cmd.Flags().Lookup("disable-cloud-controller"))
	return cmd
}

// var ServerCmd = &cobra.Command{
// 	Use:   "Server",
// 	Short: "Run management Server",
// 	Long:  `install Server`,
// 	// Uncomment the following line if your bare application
// 	// has an action associated with it:
// 	Run: func(cmd *cobra.Command, args []string) {
// 		Server.Run()
// 	},
// }
