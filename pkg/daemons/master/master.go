package master

import (
	"context"
	"crypto/x509"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/lib/tcplistener/cert"
	"github.com/xiaods/k8e/pkg/cluster"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/version"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	app2 "k8s.io/kubernetes/cmd/controller-manager/app"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
)

var (
	localhostIP        = net.ParseIP("127.0.0.1")
	requestHeaderCN    = "system:auth-proxy"
	kubeconfigTemplate = template.Must(template.New("kubeconfig").Parse(`apiVersion: v1
clusters:
- cluster:
    server: {{.URL}}
    certificate-authority: {{.CACert}}
  name: local
contexts:
- context:
    cluster: local
    namespace: default
    user: user
  name: Default
current-context: Default
kind: Config
preferences: {}
users:
- name: user
  user:
    client-certificate: {{.ClientCert}}
    client-key: {{.ClientKey}}
`))
)

func StartMaster(ctx context.Context, cfg *config.Control) error {
	var err error
	if err = master(ctx, cfg); err != nil {
		return err
	}
	return nil
}

func master(ctx context.Context, cfg *config.Control) error {
	var err error
	runtime := &config.ControlRuntime{}
	cfg.Runtime = runtime

	if err = prepare(ctx, cfg); err != nil {
		return err
	}
	_, _, err = apiServer(ctx, cfg)
	if err != nil {
		return err
	}
	if err = waitForAPIServerInBackground(ctx, runtime); err != nil {
		return err
	}
	logrus.Info("api server start success")
	return nil
}

func prepare(ctx context.Context, config *config.Control) error {
	var err error
	defaults(config)
	config.DataDir, err = filepath.Abs(config.DataDir)
	if err != nil {
		return err
	}

	os.MkdirAll(filepath.Join(config.DataDir, "tls"), 0700)
	os.MkdirAll(filepath.Join(config.DataDir, "cred"), 0700)
	runtime := config.Runtime
	runtime.ClientCA = filepath.Join(config.DataDir, "tls", "client-ca.crt")
	runtime.ClientCAKey = filepath.Join(config.DataDir, "tls", "client-ca.key")
	runtime.ServerCA = filepath.Join(config.DataDir, "tls", "server-ca.crt")
	runtime.ServerCAKey = filepath.Join(config.DataDir, "tls", "server-ca.key")
	runtime.RequestHeaderCA = filepath.Join(config.DataDir, "tls", "request-header-ca.crt")
	runtime.RequestHeaderCAKey = filepath.Join(config.DataDir, "tls", "request-header-ca.key")
	runtime.IPSECKey = filepath.Join(config.DataDir, "cred", "ipsec.psk")

	runtime.ServiceKey = filepath.Join(config.DataDir, "tls", "service.key")
	runtime.PasswdFile = filepath.Join(config.DataDir, "cred", "passwd")
	runtime.NodePasswdFile = filepath.Join(config.DataDir, "cred", "node-passwd")

	runtime.KubeConfigAdmin = filepath.Join(config.DataDir, "cred", "admin.kubeconfig")
	runtime.KubeConfigController = filepath.Join(config.DataDir, "cred", "controller.kubeconfig")
	runtime.KubeConfigScheduler = filepath.Join(config.DataDir, "cred", "scheduler.kubeconfig")
	runtime.KubeConfigAPIServer = filepath.Join(config.DataDir, "cred", "api-server.kubeconfig")
	runtime.KubeConfigCloudController = filepath.Join(config.DataDir, "cred", "cloud-controller.kubeconfig")

	runtime.ClientAdminCert = filepath.Join(config.DataDir, "tls", "client-admin.crt")
	runtime.ClientAdminKey = filepath.Join(config.DataDir, "tls", "client-admin.key")
	runtime.ClientControllerCert = filepath.Join(config.DataDir, "tls", "client-controller.crt")
	runtime.ClientControllerKey = filepath.Join(config.DataDir, "tls", "client-controller.key")
	runtime.ClientCloudControllerCert = filepath.Join(config.DataDir, "tls", "client-cloud-controller.crt")
	runtime.ClientCloudControllerKey = filepath.Join(config.DataDir, "tls", "client-cloud-controller.key")
	runtime.ClientSchedulerCert = filepath.Join(config.DataDir, "tls", "client-scheduler.crt")
	runtime.ClientSchedulerKey = filepath.Join(config.DataDir, "tls", "client-scheduler.key")
	runtime.ClientKubeAPICert = filepath.Join(config.DataDir, "tls", "client-kube-apiserver.crt")
	runtime.ClientKubeAPIKey = filepath.Join(config.DataDir, "tls", "client-kube-apiserver.key")
	runtime.ClientKubeProxyCert = filepath.Join(config.DataDir, "tls", "client-kube-proxy.crt")
	runtime.ClientKubeProxyKey = filepath.Join(config.DataDir, "tls", "client-kube-proxy.key")
	runtime.ClientK3sControllerCert = filepath.Join(config.DataDir, "tls", "client-"+version.Program+"-controller.crt")
	runtime.ClientK3sControllerKey = filepath.Join(config.DataDir, "tls", "client-"+version.Program+"-controller.key")

	runtime.ServingKubeAPICert = filepath.Join(config.DataDir, "tls", "serving-kube-apiserver.crt")
	runtime.ServingKubeAPIKey = filepath.Join(config.DataDir, "tls", "serving-kube-apiserver.key")

	runtime.ClientKubeletKey = filepath.Join(config.DataDir, "tls", "client-kubelet.key")
	runtime.ServingKubeletKey = filepath.Join(config.DataDir, "tls", "serving-kubelet.key")

	runtime.ClientAuthProxyCert = filepath.Join(config.DataDir, "tls", "client-auth-proxy.crt")
	runtime.ClientAuthProxyKey = filepath.Join(config.DataDir, "tls", "client-auth-proxy.key")

	runtime.ETCDServerCA = filepath.Join(config.DataDir, "tls", "etcd", "server-ca.crt")
	runtime.ETCDServerCAKey = filepath.Join(config.DataDir, "tls", "etcd", "server-ca.key")
	runtime.ETCDPeerCA = filepath.Join(config.DataDir, "tls", "etcd", "peer-ca.crt")
	runtime.ETCDPeerCAKey = filepath.Join(config.DataDir, "tls", "etcd", "peer-ca.key")
	runtime.ServerETCDCert = filepath.Join(config.DataDir, "tls", "etcd", "server-client.crt")
	runtime.ServerETCDKey = filepath.Join(config.DataDir, "tls", "etcd", "server-client.key")
	runtime.PeerServerClientETCDCert = filepath.Join(config.DataDir, "tls", "etcd", "peer-server-client.crt")
	runtime.PeerServerClientETCDKey = filepath.Join(config.DataDir, "tls", "etcd", "peer-server-client.key")
	runtime.ClientETCDCert = filepath.Join(config.DataDir, "tls", "etcd", "client.crt")
	runtime.ClientETCDKey = filepath.Join(config.DataDir, "tls", "etcd", "client.key")

	if config.EncryptSecrets {
		runtime.EncryptionConfig = filepath.Join(config.DataDir, "cred", "encryption-config.json")
	}
	c := cluster.New(config)
	err = c.BootstrapLoad(config)
	if err != nil {
		logrus.Error(err)
		return err
	}

	err = genCerts(config)
	if err != nil {
		logrus.Error(err)
		return err
	}

	ready, err := c.Start(ctx)
	if err != nil {
		logrus.Error(err)
		return err
	}
	runtime.ETCDReady = ready
	return nil
}

func apiServer(ctx context.Context, cfg *config.Control) (authenticator.Request, http.Handler, error) {
	argsMap := make(map[string]string)
	setEtcdStorageBackend(argsMap, cfg)
	certDir := filepath.Join(cfg.DataDir, "tls", "temporary-certs")
	os.MkdirAll(certDir, 0700)
	runtime := cfg.Runtime
	argsMap["cert-dir"] = certDir
	argsMap["allow-privileged"] = "true"
	argsMap["authorization-mode"] = strings.Join([]string{modes.ModeNode, modes.ModeRBAC}, ",")
	//argsMap["service-account-signing-key-file"] = runtime.ServiceKey
	argsMap["service-cluster-ip-range"] = cfg.ServiceIPRange.String()
	argsMap["advertise-port"] = strconv.Itoa(cfg.AdvertisePort)
	if cfg.AdvertiseIP != "" {
		argsMap["advertise-address"] = cfg.AdvertiseIP
	}
	argsMap["insecure-port"] = "8080"
	argsMap["secure-port"] = strconv.Itoa(cfg.APIServerPort)
	if cfg.APIServerBindAddress == "" {
		argsMap["bind-address"] = localhostIP.String()
	} else {
		argsMap["bind-address"] = cfg.APIServerBindAddress
	}
	// argsMap["tls-cert-file"] = runtime.ServingKubeAPICert
	// argsMap["tls-private-key-file"] = runtime.ServingKubeAPIKey
	// argsMap["service-account-key-file"] = runtime.ServiceKey
	// argsMap["service-account-issuer"] = version.Program
	// argsMap["api-audiences"] = "unknown"
	// argsMap["kubelet-certificate-authority"] = runtime.ServerCA
	// argsMap["kubelet-client-certificate"] = runtime.ClientKubeAPICert
	// argsMap["kubelet-client-key"] = runtime.ClientKubeAPIKey
	// argsMap["requestheader-client-ca-file"] = runtime.RequestHeaderCA
	// argsMap["requestheader-allowed-names"] = requestHeaderCN
	// argsMap["proxy-client-cert-file"] = runtime.ClientAuthProxyCert
	// argsMap["proxy-client-key-file"] = runtime.ClientAuthProxyKey
	argsMap["requestheader-extra-headers-prefix"] = "X-Remote-Extra-"
	argsMap["requestheader-group-headers"] = "X-Remote-Group"
	argsMap["requestheader-username-headers"] = "X-Remote-User"
	//argsMap["client-ca-file"] = runtime.ClientCA
	argsMap["enable-admission-plugins"] = "NodeRestriction"
	argsMap["anonymous-auth"] = "false"
	argsMap["profiling"] = "false"
	if cfg.EncryptSecrets {
		argsMap["encryption-provider-config"] = runtime.EncryptionConfig
	}
	args := config.GetArgsList(argsMap, cfg.ExtraAPIArgs)
	logrus.Infof("Running kube-apiserver %s", config.ArgString(args))
	return executor.APIServer(ctx, runtime.ETCDReady, args)
}

func setEtcdStorageBackend(argsMap map[string]string, cfg *config.Control) {
	argsMap["storage-backend"] = "etcd3"
	argsMap["etcd-servers"] = cfg.Datastore.Endpoint
	argsMap["etcd-cafile"] = cfg.Datastore.CAFile
	argsMap["etcd-certfile"] = cfg.Datastore.CertFile
	argsMap["etcd-keyfile"] = cfg.Datastore.KeyFile
}

func defaults(config *config.Control) {
	if config.ClusterIPRange == nil {
		_, clusterIPNet, _ := net.ParseCIDR("10.42.0.0/16")
		config.ClusterIPRange = clusterIPNet
	}

	if config.ServiceIPRange == nil {
		_, serviceIPNet, _ := net.ParseCIDR("10.43.0.0/16")
		config.ServiceIPRange = serviceIPNet
	}

	if len(config.ClusterDNS) == 0 {
		config.ClusterDNS = net.ParseIP("10.43.0.10")
	}

	if config.AdvertisePort == 0 {
		config.AdvertisePort = config.HTTPSPort
	}

	if config.APIServerPort == 0 {
		if config.HTTPSPort != 0 {
			config.APIServerPort = config.HTTPSPort + 1
		} else {
			config.APIServerPort = 6444
		}
	}

	if config.DataDir == "" {
		config.DataDir = "./management-state"
	}
}

//generate certificate
func genCerts(config *config.Control) error {
	err := genETCDCerts(config)
	if err != nil {
		return err
	}
	return nil
}

func addSANs(altNames *cert.AltNames, sans []string) {
	for _, san := range sans {
		ip := net.ParseIP(san)
		if ip == nil {
			altNames.DNSNames = append(altNames.DNSNames, san)
		} else {
			altNames.IPs = append(altNames.IPs, ip)
		}
	}
}

//generate etcd certificate
func genETCDCerts(config *config.Control) error {
	runtime := config.Runtime
	//创建CA证书
	regen, err := cert.CreateCACertKey("etcd-server", runtime.ETCDServerCA, runtime.ETCDServerCAKey)
	if err != nil {
		return nil
	}
	altNames := &cert.AltNames{
		DNSNames: []string{"localhost"},
	}
	addSANs(altNames, config.SANs)
	_, err = cert.CreateClientCertKey(regen, "etcd-server", nil, altNames, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		runtime.ETCDServerCA, runtime.ETCDServerCAKey, runtime.ServerETCDCert, runtime.ServerETCDKey)
	if err != nil {
		return err
	}
	_, err = cert.CreateClientCertKey(regen, "etcd-client", nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		runtime.ETCDServerCA, runtime.ETCDServerCAKey, runtime.ClientETCDCert, runtime.ClientETCDKey)
	if err != nil {
		return err
	}

	regen, err = cert.CreateCACertKey("etcd-peer", runtime.ETCDPeerCA, runtime.ETCDPeerCAKey)
	if err != nil {
		return nil
	}
	_, err = cert.CreateClientCertKey(regen, "etcd-peer", nil, altNames, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		runtime.ETCDPeerCA, runtime.ETCDPeerCAKey, runtime.PeerServerClientETCDCert, runtime.PeerServerClientETCDKey)
	if err != nil {
		return err
	}
	return nil
}

func waitForAPIServerInBackground(ctx context.Context, runtime *config.ControlRuntime) error {
	restConfig, err := clientcmd.BuildConfigFromFlags("", runtime.KubeConfigAdmin)
	if err != nil {
		return err
	}

	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	done := make(chan struct{})
	runtime.APIServerReady = done

	go func() {
		defer close(done)

	etcdLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-runtime.ETCDReady:
				break etcdLoop
			case <-time.After(30 * time.Second):
				logrus.Infof("Waiting for etcd server to become available")
			}
		}

		logrus.Infof("Waiting for API server to become available")
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-promise(func() error { return app2.WaitForAPIServer(k8sClient, 30*time.Second) }):
				if err != nil {
					logrus.Infof("Waiting for API server to become available")
					continue
				}
				return
			}
		}
	}()

	return nil
}

func promise(f func() error) <-chan error {
	c := make(chan error, 1)
	go func() {
		c <- f()
		close(c)
	}()
	return c
}
