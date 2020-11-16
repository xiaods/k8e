package cmds

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/version"
)

type AgentConfig struct {
	Token                    string
	TokenFile                string
	ClusterSecret            string
	ServerURL                string
	DisableLoadBalancer      bool
	ResolvConf               string
	DataDir                  string
	NodeIP                   string
	NodeExternalIP           string
	NodeName                 string
	PauseImage               string
	Snapshotter              string
	Docker                   bool
	ContainerRuntimeEndpoint string
	NoFlannel                bool
	FlannelIface             string
	FlannelConf              string
	Debug                    bool
	Rootless                 bool
	RootlessAlreadyUnshared  bool
	WithNodeID               bool
	EnableSELinux            bool
	ProtectKernelDefaults    bool
	AgentShared
	ExtraKubeletArgs   cli.StringSlice
	ExtraKubeProxyArgs cli.StringSlice
	Labels             cli.StringSlice
	Taints             cli.StringSlice
	PrivateRegistry    string

	ClusterCIDR string
	DisableCCM  bool
	DaemonURL   string
	Internal    bool //是否内嵌

}

type AgentShared struct {
	NodeIP string
}

var (
	appName = filepath.Base(os.Args[0])
	Agent   AgentConfig
)

func NewAgentCommand(run func(cmd *cobra.Command, args []string)) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "agent"
	cmd.Short = "Run node agent"
	cmd.Long = "Run node agent"
	cmd.Run = run

	cmd.Flags().StringVarP(&Agent.DataDir, "data-dir", "d", "", "(data) Folder to hold state default /var/lib/k8e/"+version.Program+" or ${HOME}/.k8e/"+version.Program+" if not root")
	cmd.Flags().StringVarP(&Agent.ServerURL, "server", "s", "", "(experimental/cluster) Server to connect to, used to join a cluster")
	cmd.Flags().BoolVar(&Agent.DisableCCM, "disable-cloud-controller", true, "(components) Disable "+version.Program+" default cloud controller manager")
	cmd.Flags().StringVar(&Agent.DaemonURL, "daemon", "", "")

	viper.BindPFlag("data-dir", cmd.Flags().Lookup("data-dir"))
	viper.BindPFlag("disable-cloud-controller", cmd.Flags().Lookup("disable-cloud-controller"))
	viper.BindPFlag("server", cmd.Flags().Lookup("server"))
	viper.BindPFlag("daemon", cmd.Flags().Lookup("daemon"))
	return cmd
}
