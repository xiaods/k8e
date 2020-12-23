package server

import (
	"context"
	"fmt"
	net2 "net"
	"os"
	"runtime"

	"github.com/erikdubbelboer/gspt"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/agent"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/netutil"
	"github.com/xiaods/k8e/pkg/rootless"
	"github.com/xiaods/k8e/pkg/server"
	"github.com/xiaods/k8e/pkg/signals"
	"github.com/xiaods/k8e/pkg/token"
	"k8s.io/apimachinery/pkg/util/net"
)

func Run(cmd *cobra.Command, args []string) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	logrus.Info("start server")
	run(&cmds.ServerConfig)
}

func run(cfg *cmds.Server) error {
	var (
		err error
	)

	// hide process arguments from ps output, since they may contain
	// database credentials or other secrets.
	gspt.SetProcTitle(os.Args[0] + " server")

	if !cfg.DisableAgent && os.Getuid() != 0 && !cfg.Rootless {
		return fmt.Errorf("must run as root unless --disable-agent is specified")
	}

	if cfg.Rootless {
		dataDir, err := datadir.LocalHome(cfg.DataDir, true)
		if err != nil {
			return err
		}
		cfg.DataDir = dataDir
		if err := rootless.Rootless(dataDir); err != nil {
			return err
		}
	}

	serverConfig := server.Config{}
	serverConfig.DisableAgent = cfg.DisableAgent
	serverConfig.ControlConfig.Token = cfg.Token
	serverConfig.ControlConfig.AgentToken = cfg.AgentToken
	serverConfig.ControlConfig.JoinURL = cfg.ServerURL
	if cfg.AgentTokenFile != "" {
		serverConfig.ControlConfig.AgentToken, err = token.ReadFile(cfg.AgentTokenFile)
		if err != nil {
			return err
		}
	}
	if cfg.TokenFile != "" {
		serverConfig.ControlConfig.Token, err = token.ReadFile(cfg.TokenFile)
		if err != nil {
			return err
		}
	}

	serverConfig.ControlConfig.DataDir = cfg.DataDir
	serverConfig.ControlConfig.KubeConfigOutput = cfg.KubeConfigOutput
	serverConfig.ControlConfig.KubeConfigMode = cfg.KubeConfigMode
	serverConfig.ControlConfig.NoScheduler = cfg.DisableScheduler
	serverConfig.Rootless = cfg.Rootless
	serverConfig.ControlConfig.SANs = knownIPs(cfg.TLSSan)
	serverConfig.ControlConfig.BindAddress = cfg.BindAddress
	serverConfig.ControlConfig.SupervisorPort = cfg.SupervisorPort
	serverConfig.ControlConfig.HTTPSPort = cfg.HTTPSPort
	serverConfig.ControlConfig.APIServerPort = cfg.APIServerPort
	serverConfig.ControlConfig.APIServerBindAddress = cfg.APIServerBindAddress
	serverConfig.ControlConfig.ExtraAPIArgs = cfg.ExtraAPIArgs
	serverConfig.ControlConfig.ExtraControllerArgs = cfg.ExtraControllerArgs
	serverConfig.ControlConfig.ExtraSchedulerAPIArgs = cfg.ExtraSchedulerArgs
	serverConfig.ControlConfig.ClusterDomain = cfg.ClusterDomain
	serverConfig.ControlConfig.Datastore.Endpoint = cfg.DatastoreEndpoint
	serverConfig.ControlConfig.Datastore.CAFile = cfg.DatastoreCAFile
	serverConfig.ControlConfig.Datastore.CertFile = cfg.DatastoreCertFile
	serverConfig.ControlConfig.Datastore.KeyFile = cfg.DatastoreKeyFile
	serverConfig.ControlConfig.AdvertiseIP = cfg.AdvertiseIP
	serverConfig.ControlConfig.AdvertisePort = cfg.AdvertisePort
	serverConfig.ControlConfig.FlannelBackend = cfg.FlannelBackend
	serverConfig.ControlConfig.ExtraCloudControllerArgs = cfg.ExtraCloudControllerArgs
	serverConfig.ControlConfig.DisableCCM = cfg.DisableCCM
	serverConfig.ControlConfig.DisableNPC = cfg.DisableNPC
	serverConfig.ControlConfig.DisableKubeProxy = cfg.DisableKubeProxy
	serverConfig.ControlConfig.ClusterInit = cfg.ClusterInit
	serverConfig.ControlConfig.EncryptSecrets = cfg.EncryptSecrets
	serverConfig.ControlConfig.EtcdSnapshotCron = cfg.EtcdSnapshotCron
	serverConfig.ControlConfig.EtcdSnapshotDir = cfg.EtcdSnapshotDir
	serverConfig.ControlConfig.EtcdSnapshotRetention = cfg.EtcdSnapshotRetention
	serverConfig.ControlConfig.EtcdDisableSnapshots = cfg.EtcdDisableSnapshots

	if cfg.ClusterResetRestorePath != "" && !cfg.ClusterReset {
		return errors.New("Invalid flag use. --cluster-reset required with --cluster-reset-restore-path")
	}

	serverConfig.ControlConfig.ClusterReset = cfg.ClusterReset
	serverConfig.ControlConfig.ClusterResetRestorePath = cfg.ClusterResetRestorePath

	if serverConfig.ControlConfig.SupervisorPort == 0 {
		serverConfig.ControlConfig.SupervisorPort = serverConfig.ControlConfig.HTTPSPort
	}

	if cmds.AgentConfig.FlannelIface != "" && cmds.AgentConfig.NodeIP == "" {
		cmds.AgentConfig.NodeIP = netutil.GetIPFromInterface(cmds.AgentConfig.FlannelIface)
	}
	if serverConfig.ControlConfig.PrivateIP == "" && cmds.AgentConfig.NodeIP != "" {
		serverConfig.ControlConfig.PrivateIP = cmds.AgentConfig.NodeIP
	}
	if serverConfig.ControlConfig.AdvertiseIP == "" && cmds.AgentConfig.NodeExternalIP != "" {
		serverConfig.ControlConfig.AdvertiseIP = cmds.AgentConfig.NodeExternalIP
	}
	if serverConfig.ControlConfig.AdvertiseIP == "" && cmds.AgentConfig.NodeIP != "" {
		serverConfig.ControlConfig.AdvertiseIP = cmds.AgentConfig.NodeIP
	}
	if serverConfig.ControlConfig.AdvertiseIP != "" {
		serverConfig.ControlConfig.SANs = append(serverConfig.ControlConfig.SANs, serverConfig.ControlConfig.AdvertiseIP)
	}

	_, serverConfig.ControlConfig.ClusterIPRange, err = net2.ParseCIDR(cfg.ClusterCIDR)
	if err != nil {
		return errors.Wrapf(err, "Invalid CIDR %s: %v", cfg.ClusterCIDR, err)
	}
	_, serverConfig.ControlConfig.ServiceIPRange, err = net2.ParseCIDR(cfg.ServiceCIDR)
	if err != nil {
		return errors.Wrapf(err, "Invalid CIDR %s: %v", cfg.ServiceCIDR, err)
	}

	_, apiServerServiceIP, err := master.ServiceIPRange(*serverConfig.ControlConfig.ServiceIPRange)
	if err != nil {
		return err
	}
	serverConfig.ControlConfig.SANs = append(serverConfig.ControlConfig.SANs, apiServerServiceIP.String())

	ctx := signals.SetupSignalHandler(context.Background())
	if err = daemons.D.StartServer(ctx, &serverConfig.ControlConfig); err != nil {
		return err
	}
	if cfg.DisableAgent {
		<-ctx.Done()
		return nil
	}
	ip := serverConfig.ControlConfig.APIServerBindAddress
	if ip == "" {
		ip = "127.0.0.1"
	}
	url := fmt.Sprintf("https://%s:%d", ip, serverConfig.ControlConfig.APIServerPort)
	daemonURL := fmt.Sprintf("http://%s:%d", ip, serverConfig.ControlConfig.APIServerPort+1)
	agentConfig := cmds.Agent
	agentConfig.ServerURL = url
	agentConfig.DataDir = datadir
	agentConfig.DaemonURL = daemonURL
	agentConfig.ClusterCIDR = cfg.ClusterCIDR
	agentConfig.DisableCCM = cfg.DisableCCM
	agentConfig.Internal = true
	return agent.InternlRun(ctx, &agentConfig)
}

func knownIPs(ips []string) []string {
	ips = append(ips, "127.0.0.1")
	ip, err := net.ChooseHostInterface()
	if err == nil {
		ips = append(ips, ip.String())
	}
	return ips
}
