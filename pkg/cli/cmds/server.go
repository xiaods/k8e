package cmds

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/xiaods/k8e/pkg/version"
)

type Server struct {
	ClusterCIDR    string
	AgentToken     string
	AgentTokenFile string
	Token          string
	TokenFile      string
	// ClusterSecret  string
	ServiceCIDR   string
	ClusterDNS    string
	ClusterDomain string
	// The port which kubectl clients can access k8s
	HTTPSPort int
	// The port which custom k3s API runs on
	SupervisorPort int
	// The port which kube-apiserver runs on
	APIServerPort            int
	APIServerBindAddress     string
	DataDir                  string
	DisableAgent             bool
	KubeConfigOutput         string
	KubeConfigMode           string
	TLSSan                   []string
	BindAddress              string
	ExtraAPIArgs             []string
	ExtraSchedulerArgs       []string
	ExtraControllerArgs      []string
	ExtraCloudControllerArgs []string
	Rootless                 bool
	DatastoreEndpoint        string
	DatastoreCAFile          string
	DatastoreCertFile        string
	DatastoreKeyFile         string
	AdvertiseIP              string
	AdvertisePort            int
	DisableScheduler         bool
	ServerURL                string
	FlannelBackend           string
	DefaultLocalStoragePath  string
	DisableCCM               bool
	DisableNPC               bool
	DisableKubeProxy         bool
	ClusterInit              bool
	ClusterReset             bool
	ClusterResetRestorePath  string
	EncryptSecrets           bool
	StartupHooks             []func(context.Context, <-chan struct{}, string) error
	EtcdDisableSnapshots     bool
	EtcdSnapshotDir          string
	EtcdSnapshotCron         string
	EtcdSnapshotRetention    int
}

var ServerConfig Server

func NewServerCommand(run func(cmd *cobra.Command, args []string)) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "server"
	cmd.Short = "Run management Server"
	cmd.Long = "Run management Server"
	cmd.Run = run
	cmd.DisableFlagParsing = true
	cmd.Flags().StringVar(&ServerConfig.BindAddress, "bind-address", "", "(listener) "+version.Program+" bind address (default: 0.0.0.0)")
	cmd.Flags().IntVar(&ServerConfig.HTTPSPort, "https-listen-port", 6443, "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")
	cmd.Flags().StringVarP(&ServerConfig.DataDir, "data-dir", "d", "", "(data) Folder to hold state default /var/lib/k8e/"+version.Program+" or ${HOME}/.k8e/"+version.Program+" if not root")
	cmd.Flags().StringVar(&ServerConfig.ClusterCIDR, "cluster-cidr", "10.42.0.0/16", "(networking) Network CIDR to use for pod IPs")
	cmd.Flags().StringVar(&ServerConfig.ServiceCIDR, "service-cidr", "10.43.0.0/16", "(networking) Network CIDR to use for services IPs")
	cmd.Flags().StringVar(&ServerConfig.ClusterDNS, "cluster-dns", "", "(networking) Cluster IP for coredns service. Should be in your service-cidr range (default: 10.43.0.10)")
	cmd.Flags().StringVar(&ServerConfig.ClusterDomain, "cluster-domain", "cluster.local", "(networking) Cluster Domain")
	cmd.Flags().StringVarP(&ServerConfig.ServerURL, "server", "s", "", "(experimental/cluster) Server to connect to, used to join a cluster")
	cmd.Flags().StringArrayVar(&ServerConfig.TLSSan, "tls-san", nil, "(listener) Add additional hostname or IP as a Subject Alternative Name in the TLS cert")
	cmd.Flags().BoolVar(&ServerConfig.DisableAgent, "disable-agent", true, "Do not run a local agent and register a local kubelet")
	cmd.Flags().BoolVar(&ServerConfig.DisableCCM, "disable-cloud-controller", true, "(components) Disable "+version.Program+" default cloud controller manager")
	cmd.Flags().StringVar(&ServerConfig.AdvertiseIP, "advertise-address", "", "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")
	cmd.Flags().BoolVar(&ServerConfig.Rootless, "rootless", false, "(experimental) Run rootless")
	cmd.Flags().StringVarP(&ServerConfig.Token, "token", "t", viper.GetString("token"), "(cluster) Shared secret used to join a server or agent to a cluster (ENV: "+version.ProgramUpper+"_TOKEN"+")")
	cmd.Flags().StringVar(&ServerConfig.TokenFile, "token-file", viper.GetString("token-file"), "(cluster) File containing the cluster-secret/token (ENV: "+version.ProgramUpper+"_TOKEN_FILE"+")")
	cmd.Flags().StringVar(&ServerConfig.AgentToken, "agent-token", viper.GetString("agent-token"), "(experimental/cluster) Shared secret used to join agents to the cluster, but not servers (ENV: "+version.ProgramUpper+"_AGENT_TOKEN"+")")
	cmd.Flags().StringVar(&ServerConfig.AgentTokenFile, "agent-token-file", viper.GetString("agent-token-file"), "(experimental/cluster) File containing the agent secret (ENV: "+version.ProgramUpper+"_AGENT_TOKEN_FILE"+")")

	viper.SetEnvPrefix(version.ProgramUpper)
	viper.BindPFlag("bind-address", cmd.Flags().Lookup("bind-address"))
	viper.BindPFlag("https-listen-port", cmd.Flags().Lookup("https-listen-port"))
	viper.BindPFlag("data-dir", cmd.Flags().Lookup("data-dir"))
	viper.BindPFlag("cluster-cidr", cmd.Flags().Lookup("cluster-cidr"))
	viper.BindPFlag("service-cidr", cmd.Flags().Lookup("service-cidr"))
	viper.BindPFlag("cluster-dns", cmd.Flags().Lookup("cluster-dns"))
	viper.BindPFlag("cluster-domain", cmd.Flags().Lookup("cluster-domain"))
	viper.BindPFlag("server", cmd.Flags().Lookup("server"))
	viper.BindPFlag("tls-san", cmd.Flags().Lookup("tls-san"))
	viper.BindPFlag("disable-agent", cmd.Flags().Lookup("disable-agent"))
	viper.BindPFlag("disable-cloud-controller", cmd.Flags().Lookup("disable-cloud-controller"))
	viper.BindPFlag("advertise-address", cmd.Flags().Lookup("advertise-address"))
	viper.BindPFlag("rootless", cmd.Flags().Lookup("rootless"))
	viper.BindPFlag("token", cmd.Flags().Lookup("token"))
	viper.BindEnv("token", "TOKEN")
	viper.BindPFlag("token-file", cmd.Flags().Lookup("token-file"))
	viper.BindEnv("token-file", "TOKEN_FILE")
	viper.BindPFlag("agent-token", cmd.Flags().Lookup("agent-token"))
	viper.BindEnv("agent-token", "AGENT_TOKEN")
	viper.BindPFlag("agent-token-file", cmd.Flags().Lookup("agent-token-file"))
	viper.BindEnv("agent-token-file", "AGENT_TOKEN_FILE")

	return cmd
}
