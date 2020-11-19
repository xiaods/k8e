package cmds

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/xiaods/k8e/pkg/version"
)

type ServerConfig struct {
	ClusterCIDR    string
	AgentToken     string
	AgentTokenFile string
	Token          string
	TokenFile      string
	ClusterSecret  string
	ServiceCIDR    string
	ClusterDNS     string
	ClusterDomain  string
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

var Server ServerConfig

func NewServerCommand(run func(cmd *cobra.Command, args []string)) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "server"
	cmd.Short = "Run management Server"
	cmd.Long = "Run management Server"
	cmd.Run = run
	cmd.Flags().StringVar(&Server.BindAddress, "bind-address", "", "(listener) "+version.Program+" bind address (default: 0.0.0.0)")
	cmd.Flags().IntVar(&Server.HTTPSPort, "https-listen-port", 6443, "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")
	cmd.Flags().StringVarP(&Server.DataDir, "data-dir", "d", "", "(data) Folder to hold state default /var/lib/k8e/"+version.Program+" or ${HOME}/.k8e/"+version.Program+" if not root")
	cmd.Flags().StringVar(&Server.ClusterCIDR, "cluster-cidr", "10.42.0.0/16", "(networking) Network CIDR to use for pod IPs")
	cmd.Flags().StringVar(&Server.ServiceCIDR, "service-cidr", "10.43.0.0/16", "(networking) Network CIDR to use for services IPs")
	cmd.Flags().StringVar(&Server.ClusterDNS, "cluster-dns", "", "(networking) Cluster IP for coredns service. Should be in your service-cidr range (default: 10.43.0.10)")
	cmd.Flags().StringVar(&Server.ClusterDomain, "cluster-domain", "cluster.local", "(networking) Cluster Domain")
	cmd.Flags().StringVarP(&Server.ServerURL, "server", "s", "", "(experimental/cluster) Server to connect to, used to join a cluster")
	cmd.Flags().StringArrayVar(&Server.TLSSan, "tls-san", nil, "(listener) Add additional hostname or IP as a Subject Alternative Name in the TLS cert")
	cmd.Flags().BoolVar(&Server.DisableAgent, "disable-agent", true, "Do not run a local agent and register a local kubelet")
	cmd.Flags().BoolVar(&Server.DisableCCM, "disable-cloud-controller", true, "(components) Disable "+version.Program+" default cloud controller manager")
	cmd.Flags().StringVar(&Server.AdvertiseIP, "advertise-address", "", "(listener) IP address that apiserver uses to advertise to members of the cluster (default: node-external-ip/node-ip)")

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
	return cmd
}
