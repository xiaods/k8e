package spegel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	pkgerrors "github.com/pkg/errors"
	"github.com/rancher/wrangler/v3/pkg/merr"
	"github.com/sirupsen/logrus"
	"github.com/spegel-org/spegel/pkg/routing"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/util"
	"github.com/xiaods/k8e/pkg/version"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	nodeutil "k8s.io/kubernetes/pkg/controller/util/node"
)

// explicit interface checks
var _ routing.Bootstrapper = &selfBootstrapper{}
var _ routing.Bootstrapper = &notSelfBootstrapper{}
var _ routing.Bootstrapper = &agentBootstrapper{}
var _ routing.Bootstrapper = &serverBootstrapper{}
var _ routing.Bootstrapper = &chainingBootstrapper{}

type selfBootstrapper struct {
	id *peer.AddrInfo
}

// NewSelfBootstrapper returns a stub p2p bootstrapper that just returns its own ID
func NewSelfBootstrapper() routing.Bootstrapper {
	return &selfBootstrapper{}
}

func (s *selfBootstrapper) Run(ctx context.Context, id peer.AddrInfo) error {
	s.id = &id
	return waitForDone(ctx)
}

func (s *selfBootstrapper) Get(ctx context.Context) ([]peer.AddrInfo, error) {
	if s.id == nil {
		return nil, errors.New("p2p peer not ready")
	}
	return []peer.AddrInfo{*s.id}, nil
}

type notSelfBootstrapper struct {
	id *peer.AddrInfo
	b  routing.Bootstrapper
}

// NewNotSelfBootstrapper wraps an existing bootstrapper,
// and will never return a list of peers containing only itself.
func NewNotSelfBootstrapper(b routing.Bootstrapper) routing.Bootstrapper {
	return &notSelfBootstrapper{
		b: b,
	}
}

func (ns *notSelfBootstrapper) Run(ctx context.Context, id peer.AddrInfo) error {
	ns.id = &id
	return ns.b.Run(ctx, id)
}

func (ns *notSelfBootstrapper) Get(ctx context.Context) ([]peer.AddrInfo, error) {
	peers, err := ns.b.Get(ctx)
	if err == nil && len(peers) == 1 && ns.id != nil && peers[0].ID == ns.id.ID {
		return nil, nil
	}
	return peers, err
}

type agentBootstrapper struct {
	server     string
	token      string
	clientCert string
	clientKey  string
	info       *clientaccess.Info
}

// NewAgentBootstrapper returns a p2p bootstrapper that retrieves a peer address from its server
func NewAgentBootstrapper(server, token, dataDir string) routing.Bootstrapper {
	return &agentBootstrapper{
		clientCert: filepath.Join(dataDir, "agent", "client-kubelet.crt"),
		clientKey:  filepath.Join(dataDir, "agent", "client-kubelet.key"),
		server:     server,
		token:      token,
	}
}

func (c *agentBootstrapper) Run(ctx context.Context, id peer.AddrInfo) error {
	if c.server != "" && c.token != "" {
		withCert := clientaccess.WithClientCertificate(c.clientCert, c.clientKey)
		info, err := clientaccess.ParseAndValidateToken(c.server, c.token, withCert)
		if err != nil {
			return pkgerrors.WithMessage(err, "failed to validate join token")
		}
		c.info = info
	}

	go wait.PollUntilContextCancel(ctx, 1*time.Second, true, func(ctx context.Context) (bool, error) {
		nodeName := os.Getenv("NODE_NAME")
		if nodeName == "" {
			return false, nil
		}
		address := fmt.Sprintf("%s/p2p/%s", id.Addrs[0].String(), id.ID.String())
		logrus.Infof("Node P2P address annotations and labels added: %s", address)
		return true, nil
	})
	return waitForDone(ctx)
}

func (c *agentBootstrapper) Get(ctx context.Context) ([]peer.AddrInfo, error) {
	if c.server == "" || c.token == "" {
		return nil, errors.New("cannot get addresses without server and token")
	}

	if c.info == nil {
		return nil, errors.New("client not ready")
	}

	addr, err := c.info.Get("/v1-" + version.Program + "/p2p")
	if err != nil {
		return nil, err
	}

	// If the response cannot be decoded as a JSON list of addresses, fall back
	// to using it as a legacy single-address response.
	var addrs []string
	if err := json.Unmarshal(addr, &addrs); err != nil {
		addrs = append(addrs, string(addr))
	}

	var addrInfos []peer.AddrInfo
	for _, addr := range addrs {
		if addrInfo, err := peer.AddrInfoFromString(addr); err == nil {
			addrInfos = append(addrInfos, *addrInfo)
		}
	}
	return addrInfos, nil
}

type serverBootstrapper struct {
	controlConfig *config.Control
}

// NewServerBootstrapper returns a p2p bootstrapper that returns an address from the Kubernetes node list
func NewServerBootstrapper(controlConfig *config.Control) routing.Bootstrapper {
	return &serverBootstrapper{
		controlConfig: controlConfig,
	}
}

func (s *serverBootstrapper) Run(ctx context.Context, id peer.AddrInfo) error {
	return waitForDone(ctx)
}

func (s *serverBootstrapper) Get(ctx context.Context) ([]peer.AddrInfo, error) {
	if s.controlConfig.Runtime.Core == nil {
		return nil, util.ErrCoreNotReady
	}
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, errors.New("node name not set")
	}

	nodes := s.controlConfig.Runtime.Core.Core().V1().Node()
	labelSelector := labels.Set{P2pEnabledLabel: "true"}.AsSelector()
	nodeList, err := nodes.Cache().List(labelSelector)
	if err != nil {
		return nil, err
	}

	var addrs []peer.AddrInfo
	for _, node := range nodeList {
		if node.Name == nodeName {
			// don't return our own address
			continue
		}
		if find, condition := nodeutil.GetNodeCondition(&node.Status, v1.NodeReady); find == -1 || condition.Status != v1.ConditionTrue {
			// don't return the address of a not-ready node
			continue
		}
		if val, ok := node.Annotations[P2pMulAddrAnnotation]; ok {
			info := &peer.AddrInfo{}
			if err := info.UnmarshalJSON([]byte(val)); err == nil {
				addrs = append(addrs, *info)
			}
		}
		if val, ok := node.Annotations[P2pAddressAnnotation]; ok {
			for _, addr := range strings.Split(val, ",") {
				if info, err := peer.AddrInfoFromString(addr); err == nil {
					addrs = append(addrs, *info)
				}
			}
		}
	}
	return addrs, nil
}

type chainingBootstrapper struct {
	bootstrappers []routing.Bootstrapper
}

// NewChainingBootstrapper returns a p2p bootstrapper that passes through to a list of bootstrappers.
func NewChainingBootstrapper(bootstrappers ...routing.Bootstrapper) routing.Bootstrapper {
	return &chainingBootstrapper{
		bootstrappers: bootstrappers,
	}
}

func (c *chainingBootstrapper) Run(ctx context.Context, id peer.AddrInfo) error {
	eg, ctx := errgroup.WithContext(ctx)
	for i := range c.bootstrappers {
		b := c.bootstrappers[i]
		eg.Go(func() error {
			return b.Run(ctx, id)
		})
	}
	return eg.Wait()
}

func (c *chainingBootstrapper) Get(ctx context.Context) ([]peer.AddrInfo, error) {
	errs := merr.Errors{}
	for i := range c.bootstrappers {
		b := c.bootstrappers[i]
		as, err := b.Get(ctx)
		if err != nil {
			errs = append(errs, err)
		} else if len(as) != 0 {
			return as, nil
		}
	}
	return nil, merr.NewErrors(errs...)
}

func waitForDone(ctx context.Context) error {
	<-ctx.Done()
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
