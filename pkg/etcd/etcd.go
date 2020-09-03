package etcd

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
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
	dataDir string
	name    string
	address string
}

func New() *ETCD {
	e := &ETCD{}
	address, err := getAdvertiseAddress("")
	if err != nil {
		return nil
	}
	e.dataDir = "./manager-state"
	e.address = address
	e.setName()
	return e
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

func (e *ETCD) Start() error {
	return e.newCluster()
}

func (e *ETCD) newCluster() error {

	options := InitialOptions{
		AdvertisePeerURL: fmt.Sprintf("http://%s:2380", e.address),
		Cluster:          fmt.Sprintf("%s=http://%s:2380", e.name, e.address),
		State:            "new",
	}
	config := ETCDConfig{
		Name:                e.name,
		DataDir:             dataDir(e.dataDir),
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
	fileName := nameFile(e.dataDir)
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
		fmt.Println(err)
		return err
	}
	cfg, err := embed.ConfigFromFile(configFile)
	if err != nil {
		log.Println(err)
		return err
	}
	log.Println("start etcd...")
	log.Println("name", cfg.Name)
	log.Println("data dir", cfg.Dir)
	log.Println("ListenMetricsUrlsJSON", cfg.ListenMetricsUrlsJSON)
	//cfg.Dir = dataDir(e.dataDir)
	etcd, err := embed.StartEtcd(cfg)
	if err != nil {
		log.Println(err)
		return nil
	}
	go func() {
		err := <-etcd.Err()
		log.Println("etcd exited: ", err)
	}()
	fmt.Println("run etcd success")
	return nil
}
