package etcd

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	certutil "github.com/xiaods/k8e/lib/dynamiclistener/cert"
	"github.com/xiaods/k8e/pkg/daemons/config"
	etcd "go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/embed"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"sigs.k8s.io/yaml"
)

type ETCDConfig struct {
	InitialOptions      `json:",inline"`
	Name                string `json:"name,omitempty"`
	ListenClientURLs    string `json:"listen-client-urls,omitempty"`
	ListenMetricsUrls   string `json:"listen-metrics-urls,omitempty"`
	ListenPeerURLs      string `json:"listen-peer-urls,omitempty"`
	AdvertiseClientURLs string `json:"advertise-client-urls,omitempty"`
	DataDir             string `json:"data-dir,omitempty"`

	SnapshotCount     int         `json:"snapshot-count,omitempty"`
	ServerTrust       ServerTrust `json:"client-transport-security"`
	PeerTrust         PeerTrust   `json:"peer-transport-security"`
	ForceNewCluster   bool        `json:"force-new-cluster,omitempty"`
	HeartbeatInterval int         `json:"heartbeat-interval"`
	ElectionTimeout   int         `json:"election-timeout"`
}

func (e ETCDConfig) ToConfigFile() (string, error) {
	confFile := filepath.Join(e.DataDir, "config")
	bytes, err := yaml.Marshal(&e)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(e.DataDir, 0700); err != nil {
		return "", err
	}
	return confFile, ioutil.WriteFile(confFile, bytes, 0600)
}

type ServerTrust struct {
	CertFile       string `json:"cert-file"`
	KeyFile        string `json:"key-file"`
	ClientCertAuth bool   `json:"client-cert-auth"`
	TrustedCAFile  string `json:"trusted-ca-file"`
}

type PeerTrust struct {
	CertFile       string `json:"cert-file"`
	KeyFile        string `json:"key-file"`
	ClientCertAuth bool   `json:"client-cert-auth"`
	TrustedCAFile  string `json:"trusted-ca-file"`
}

type InitialOptions struct {
	AdvertisePeerURL string `json:"initial-advertise-peer-urls,omitempty"`
	Cluster          string `json:"initial-cluster,omitempty"`
	State            string `json:"initial-cluster-state,omitempty"`
}

type ETCD struct {
	client *etcd.Client
	//dataDir string
	config  *config.Control
	name    string
	address string
}

func New(config *config.Control) *ETCD {
	e := &ETCD{}
	e.config = config
	return e
}

func newClient(ctx context.Context, runtime *config.ControlRuntime) (*etcd.Client, error) {
	// tlsConfig, err := toTLSConfig(runtime)
	// if err != nil {
	// 	return nil, err
	// }

	cfg := etcd.Config{
		Context:   ctx,
		Endpoints: []string{endpoint},
		TLS:       nil,
	}

	return etcd.New(cfg)
}

func toTLSConfig(runtime *config.ControlRuntime) (*tls.Config, error) {
	clientCert, err := tls.LoadX509KeyPair(runtime.ClientETCDCert, runtime.ClientETCDKey)
	if err != nil {
		return nil, err
	}

	pool, err := certutil.NewPool(runtime.ETCDServerCA)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
	}, nil
}

func getAdvertiseAddress(advertiseIP string) (string, error) {
	ip := advertiseIP
	if ip == "" {
		ipAddr, err := utilnet.ChooseHostInterface()
		if err != nil {
			return "", err
		}
		ip = ipAddr.String()
	}
	return ip, nil
}

func (e *ETCD) peerURL() string {
	return fmt.Sprintf("http://%s:2380", e.address)
}

func (e *ETCD) clientURL() string {
	return fmt.Sprintf("http://%s:2379", e.address)
}

func (e *ETCD) InitDB(ctx context.Context) error {
	return e.Register(ctx)
}

func (e *ETCD) Register(ctx context.Context) error {
	client, err := newClient(ctx, e.config.Runtime)
	if err != nil {
		return err
	}
	e.client = client
	address, err := getAdvertiseAddress(e.config.AdvertiseIP)
	if err != nil {
		return err
	}
	e.address = address
	e.setName()
	return nil
}

func (e *ETCD) Start(ctx context.Context) error {
	return e.newCluster()
}

const (
	snapshotPrefix = "etcd-snapshot-"
	endpoint       = "http://127.0.0.1:2379"

	testTimeout = time.Second * 10
)

func (e *ETCD) Test(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	status, err := e.client.Status(ctx, endpoint)
	if err != nil {
		return err
	}

	if status.IsLearner {
		logrus.Info("is learner")
		// if err := e.promoteMember(ctx, clientAccessInfo); err != nil {
		// 	return err
		// }
	}
	members, err := e.client.MemberList(ctx)
	if err != nil {
		return err
	}

	var memberNameUrls []string
	for _, member := range members.Members {
		for _, peerURL := range member.PeerURLs {
			if peerURL == e.peerURL() && e.name == member.Name {
				return nil
			}
		}
		if len(member.PeerURLs) > 0 {
			memberNameUrls = append(memberNameUrls, member.Name+"="+member.PeerURLs[0])
		}
	}
	msg := fmt.Sprintf("This server is a not a member of the etcd cluster. Found %v, expect: %s=%s", memberNameUrls, e.name, e.address)
	logrus.Error(msg)
	return fmt.Errorf(msg)
}

// func (e *ETCD) promoteMember(ctx context.Context, clientAccessInfo *clientaccess.Info) error {
// 	clientURLs, _, err := e.clientURLs(ctx, clientAccessInfo)
// 	if err != nil {
// 		return err
// 	}
// 	memberPromoted := true
// 	t := time.NewTicker(5 * time.Second)
// 	defer t.Stop()
// 	for range t.C {
// 		client, err := joinClient(ctx, e.runtime, clientURLs)
// 		// continue on errors to keep trying to promote member
// 		// grpc error are shown so no need to re log them
// 		if err != nil {
// 			continue
// 		}
// 		members, err := client.MemberList(ctx)
// 		if err != nil {
// 			continue
// 		}
// 		for _, member := range members.Members {
// 			// only one learner can exist in the cluster
// 			if !member.IsLearner {
// 				continue
// 			}
// 			if _, err := client.MemberPromote(ctx, member.ID); err != nil {
// 				memberPromoted = false
// 				break
// 			}
// 		}
// 		if memberPromoted {
// 			break
// 		}
// 	}
// 	return nil
// }

func (e *ETCD) newCluster() error {

	options := InitialOptions{
		AdvertisePeerURL: fmt.Sprintf("http://%s:2380", e.address),
		Cluster:          fmt.Sprintf("%s=http://%s:2380", e.name, e.address),
		State:            "new",
	}
	config := ETCDConfig{
		Name:                e.name,
		DataDir:             dataDir(e.config.DataDir),
		InitialOptions:      options,
		ForceNewCluster:     false,
		ListenClientURLs:    fmt.Sprintf(e.clientURL() + ",http://127.0.0.1:2379"),
		ListenMetricsUrls:   fmt.Sprintf("http://127.0.0.1:2381"),
		ListenPeerURLs:      e.peerURL(),
		AdvertiseClientURLs: e.clientURL(),
		// ServerTrust: executor.ServerTrust{
		// 	CertFile:       e.config.Runtime.ServerETCDCert,
		// 	KeyFile:        e.config.Runtime.ServerETCDKey,
		// 	ClientCertAuth: true,
		// 	TrustedCAFile:  e.config.Runtime.ETCDServerCA,
		// },
		// PeerTrust: executor.PeerTrust{
		// 	CertFile:       e.config.Runtime.PeerServerClientETCDCert,
		// 	KeyFile:        e.config.Runtime.PeerServerClientETCDKey,
		// 	ClientCertAuth: true,
		// 	TrustedCAFile:  e.config.Runtime.ETCDPeerCA,
		// },
		ElectionTimeout:   5000,
		HeartbeatInterval: 500,
	}
	return e.run(config)
}

func dataDir(dataDir string) string {
	return filepath.Join(dataDir, "db", "etcd")
}

func nameFile(dataDir string) string {
	return filepath.Join(dataDir, "name")
}

func (e *ETCD) setName() error {
	fileName := nameFile(e.config.DataDir)
	data, err := ioutil.ReadFile(fileName)
	if os.IsNotExist(err) {
		h, err := os.Hostname()
		if err != nil {
			return err
		}
		e.name = strings.SplitN(h, ".", 2)[0] + "-" + uuid.New().String()[:8]
		if err := os.MkdirAll(filepath.Dir(fileName), 0755); err != nil {
			return err
		}
		return ioutil.WriteFile(fileName, []byte(e.name), 0655)
	} else if err != nil {
		return err
	}
	e.name = string(data)
	return nil
}

//Run etcd run
func (e *ETCD) run(args ETCDConfig) error {
	configFile, err := args.ToConfigFile()
	if err != nil {
		logrus.Error(err)
		return err
	}
	cfg, err := embed.ConfigFromFile(configFile)
	if err != nil {
		logrus.Error(err)
		return err
	}
	logrus.Info("start etcd...")
	logrus.Info("name", cfg.Name)
	logrus.Info("data dir", cfg.Dir)
	logrus.Info("ListenMetricsUrlsJSON", cfg.ListenMetricsUrlsJSON)
	//cfg.Dir = dataDir(e.dataDir)
	etcd, err := embed.StartEtcd(cfg)
	if err != nil {
		logrus.Error(err)
		return nil
	}
	go func() {
		err := <-etcd.Err()
		logrus.Info("etcd exited: ", err)
	}()
	logrus.Info("run etcd success")
	return nil
}
