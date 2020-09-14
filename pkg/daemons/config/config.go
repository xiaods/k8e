package config

import (
	"context"
	"net"
	"net/http"
)

type Control struct {
	AdvertisePort int
	AdvertiseIP   string
	// The port which kubectl clients can access k8s
	HTTPSPort int
	// The port which custom k3s API runs on
	SupervisorPort int
	// The port which kube-apiserver runs on
	APIServerPort        int
	APIServerBindAddress string
	AgentToken           string `json:"-"`
	Token                string `json:"-"`
	ClusterIPRange       *net.IPNet
	ServiceIPRange       *net.IPNet
	ClusterDNS           net.IP
	ClusterDomain        string
	NoCoreDNS            bool
	KubeConfigOutput     string
	KubeConfigMode       string
	DataDir              string
	Skips                map[string]bool
	Disables             map[string]bool
	//Datastore                endpoint.Config
	NoScheduler              bool
	ExtraAPIArgs             []string
	ExtraControllerArgs      []string
	ExtraCloudControllerArgs []string
	ExtraSchedulerAPIArgs    []string
	NoLeaderElect            bool
	JoinURL                  string
	FlannelBackend           string
	IPSECPSK                 string
	DefaultLocalStoragePath  string
	DisableCCM               bool
	DisableNPC               bool
	DisableKubeProxy         bool
	ClusterInit              bool
	ClusterReset             bool
	ClusterResetRestorePath  string
	EncryptSecrets           bool
	TLSMinVersion            uint16
	TLSCipherSuites          []uint16
	EtcdDisableSnapshots     bool
	EtcdSnapshotDir          string
	EtcdSnapshotCron         string
	EtcdSnapshotRetention    int

	BindAddress string
	SANs        []string

	Runtime *ControlRuntime `json:"-"`
}

type ControlRuntimeBootstrap struct {
	ETCDServerCA       string
	ETCDServerCAKey    string
	ETCDPeerCA         string
	ETCDPeerCAKey      string
	ServerCA           string
	ServerCAKey        string
	ClientCA           string
	ClientCAKey        string
	ServiceKey         string
	PasswdFile         string
	RequestHeaderCA    string
	RequestHeaderCAKey string
	IPSECKey           string
	EncryptionConfig   string
}

type ControlRuntime struct {
	ControlRuntimeBootstrap

	HTTPBootstrap          bool
	APIServerReady         <-chan struct{}
	ETCDReady              <-chan struct{}
	ClusterControllerStart func(ctx context.Context) error

	ClientKubeAPICert string
	ClientKubeAPIKey  string
	NodePasswdFile    string

	KubeConfigAdmin           string
	KubeConfigController      string
	KubeConfigScheduler       string
	KubeConfigAPIServer       string
	KubeConfigCloudController string

	ServingKubeAPICert string
	ServingKubeAPIKey  string
	ServingKubeletKey  string
	ServerToken        string
	AgentToken         string
	Handler            http.Handler
	Tunnel             http.Handler
	//Authenticator      authenticator.Request

	ClientAuthProxyCert string
	ClientAuthProxyKey  string

	ClientAdminCert           string
	ClientAdminKey            string
	ClientControllerCert      string
	ClientControllerKey       string
	ClientSchedulerCert       string
	ClientSchedulerKey        string
	ClientKubeProxyCert       string
	ClientKubeProxyKey        string
	ClientKubeletKey          string
	ClientCloudControllerCert string
	ClientCloudControllerKey  string
	ClientK3sControllerCert   string
	ClientK3sControllerKey    string

	ServerETCDCert           string
	ServerETCDKey            string
	PeerServerClientETCDCert string
	PeerServerClientETCDKey  string
	ClientETCDCert           string
	ClientETCDKey            string
}
