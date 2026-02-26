package spegel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/containerd/containerd/remotes/docker"
	"github.com/rancher/dynamiclistener/cert"
	"github.com/xiaods/k8e/pkg/agent/https"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"

	"github.com/go-logr/logr"
	"github.com/go-logr/stdr"
	"github.com/gorilla/mux"
	ipfslog "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	pkgerrors "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spegel-org/spegel/pkg/metrics"
	"github.com/spegel-org/spegel/pkg/oci"
	"github.com/spegel-org/spegel/pkg/registry"
	"github.com/spegel-org/spegel/pkg/routing"
	"github.com/spegel-org/spegel/pkg/state"
	"k8s.io/component-base/metrics/legacyregistry"
)

// DefaultRegistry is the default instance of a Spegel distributed registry
var DefaultRegistry = &Config{
	Bootstrapper: NewSelfBootstrapper(),
	Router: func(context.Context, *config.Node) (*mux.Router, error) {
		return nil, errors.New("not implemented")
	},
}

var (
	P2pAddressAnnotation = "p2p." + version.Program + ".cattle.io/node-address"
	P2pMulAddrAnnotation = "p2p." + version.Program + ".cattle.io/node-addresses"
	P2pEnabledLabel      = "p2p." + version.Program + ".cattle.io/enabled"
	P2pPortEnv           = version.ProgramUpper + "_P2P_PORT"
	P2pEnableLatestEnv   = version.ProgramUpper + "_P2P_ENABLE_LATEST"

	resolveLatestTag = false
)

// Config holds fields for a distributed registry
type Config struct {
	ClientCAFile   string
	ClientCertFile string
	ClientKeyFile  string

	ServerCAFile   string
	ServerCertFile string
	ServerKeyFile  string

	// ExternalAddress is the address for other nodes to connect to the registry API.
	ExternalAddress string

	// InternalAddress is the address for the local containerd instance to connect to the registry API.
	InternalAddress string

	// RegistryPort is the port for the registry API.
	RegistryPort string

	// PSK is the preshared key required to join the p2p network.
	PSK []byte

	// Bootstrapper is the bootstrapper that will be used to discover p2p peers.
	Bootstrapper routing.Bootstrapper

	// HandlerFunc will be called to add the registry API handler to an existing router.
	Router https.RouterFunc

	router *routing.P2PRouter
}

// These values are not currently configurable
const (
	resolveRetries    = 3
	resolveTimeout    = time.Second * 5
	registryNamespace = "k8s.io"
	defaultRouterPort = "5001"
)

func init() {
	// ensure that spegel exposes metrics through the same registry used by Kubernetes components
	metrics.DefaultRegisterer = legacyregistry.Registerer()
	metrics.DefaultGatherer = legacyregistry.DefaultGatherer
}

// configureP2PLogging sets up logging for the P2P/IPFS subsystem and returns an updated context.
func configureP2PLogging(ctx context.Context) context.Context {
	level := ipfslog.LevelInfo
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		level = ipfslog.LevelDebug
		stdlog := log.New(logrus.StandardLogger().Writer(), "spegel ", log.LstdFlags)
		logger := stdr.NewWithOptions(stdlog, stdr.Options{Verbosity: ptr.To(10)})
		ctx = logr.NewContext(ctx, logger)
	}
	ipfslog.SetAllLoggers(level)
	return ctx
}

// loadP2PKey loads or generates the persistent private key used for P2P identity.
func loadP2PKey(nodeConfig *config.Node) (crypto.PrivKey, error) {
	keyFile := filepath.Join(nodeConfig.Containerd.Opt, "peer.key")
	keyBytes, _, err := cert.LoadOrGenerateKeyFile(keyFile, false)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "failed to load or generate p2p private key")
	}
	privKey, err := cert.ParsePrivateKeyPEM(keyBytes)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "failed to parse p2p private key")
	}
	p2pKey, _, err := crypto.KeyPairFromStdKey(privKey)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "failed to convert p2p private key")
	}
	return p2pKey, nil
}

// getP2PRouterPort returns the P2P router port from the environment, or the default port.
func getP2PRouterPort() string {
	if env := os.Getenv(P2pPortEnv); env != "" {
		if i, err := strconv.Atoi(env); i == 0 || err != nil {
			logrus.Warnf("Invalid %s value; using default %v", P2pPortEnv, defaultRouterPort)
		} else {
			return env
		}
	}
	return defaultRouterPort
}

// buildRegistryList returns a deduplicated list of registries to distribute, excluding the local
// address and any invalid or localhost hosts.
func buildRegistryList(localAddr string, nodeConfig *config.Node) []string {
	var registries []string
	for host := range nodeConfig.AgentConfig.Registry.Mirrors {
		if host == localAddr {
			continue
		}
		if _, err := url.Parse("https://" + host); err != nil || docker.IsLocalhost(host) {
			logrus.Errorf("Distributed registry mirror skipping invalid registry: %s", host)
		} else {
			registries = append(registries, host)
		}
	}
	return registries
}

// applyLatestTagOverride reads P2pEnableLatestEnv and, if set to a valid bool, overrides
// the resolveLatestTag package variable.
func applyLatestTagOverride() {
	env := os.Getenv(P2pEnableLatestEnv)
	if env == "" {
		return
	}
	if b, err := strconv.ParseBool(env); err != nil {
		logrus.Warnf("Invalid %s value; using default %v", P2pEnableLatestEnv, resolveLatestTag)
	} else {
		resolveLatestTag = b
	}
}

// startStateTracker tracks OCI image state in containerd and publishes it via the p2p router.
// It restarts automatically on non-cancellation errors. Call with 'go'.
func (c *Config) startStateTracker(ctx context.Context, ociStore DeferredStore) {
	defer ociStore.Close()
	for {
		logrus.Debug("Starting embedded registry image state tracker")
		if err := ociStore.Start(); err != nil {
			logrus.Errorf("Failed to start deferred OCI store: %v", err)
		}
		err := state.Track(ctx, ociStore, c.router)
		if err != nil && errors.Is(err, context.Canceled) {
			return
		}
		logrus.Errorf("Embedded registry image state tracker exited: %v", err)
		time.Sleep(time.Second)
	}
}

// Start starts the embedded p2p router, and binds the registry API to an existing HTTP router.
func (c *Config) Start(ctx context.Context, nodeConfig *config.Node) error {
	localAddr := net.JoinHostPort(c.InternalAddress, c.RegistryPort)
	// distribute images for all configured mirrors. there doesn't need to be a
	// configured endpoint, just having a key for the registry will do.
	registries := buildRegistryList(localAddr, nodeConfig)

	if len(registries) == 0 {
		logrus.Errorf("Not starting distributed registry mirror: no registries configured for distributed mirroring")
		return nil
	}

	logrus.Infof("Starting distributed registry mirror at https://%s:%s/v2 for registries %v",
		c.ExternalAddress, c.RegistryPort, registries)

	ctx = configureP2PLogging(ctx)

	// Get containerd client
	storeOpts := []oci.ContainerdOption{oci.WithContentPath(filepath.Join(nodeConfig.Containerd.Root, "io.containerd.content.v1.content"))}
	ociStore, err := NewDeferredContainerd(ctx, nodeConfig.Containerd.Address, registryNamespace, storeOpts...)
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to create OCI store")
	}

	ociClient, err := oci.NewClient()
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to create OCI client")
	}

	// create or load persistent private key
	p2pKey, err := loadP2PKey(nodeConfig)
	if err != nil {
		return err
	}

	// create an in-memory peerstore for p2p peer discovery
	ps, err := pstoremem.NewPeerstore()
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to create peerstore")
	}

	// get latest tag configuration override
	applyLatestTagOverride()

	routerPort := getP2PRouterPort()
	routerAddr := net.JoinHostPort(c.ExternalAddress, routerPort)

	logrus.Infof("Starting distributed registry P2P node at %s", routerAddr)
	opts := []routing.P2PRouterOption{
		routing.WithLibP2POptions(
			libp2p.Identity(p2pKey),
			libp2p.Peerstore(ps),
			libp2p.PrivateNetwork(c.PSK),
		),
	}
	c.router, err = routing.NewP2PRouter(ctx, routerAddr, NewNotSelfBootstrapper(c.Bootstrapper), c.RegistryPort, opts...)
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to create P2P router")
	}
	go c.router.Run(ctx)

	metrics.Register()
	registryOpts := []registry.RegistryOption{
		registry.WithResolveRetries(resolveRetries),
		registry.WithResolveTimeout(resolveTimeout),
		registry.WithOCIClient(ociClient),
	}
	reg, err := registry.NewRegistry(ociStore, c.router, registryOpts...)
	if err != nil {
		return pkgerrors.WithMessage(err, "failed to create embedded registry")
	}
	regSvr := &http.Server{
		Addr:              ":" + c.RegistryPort,
		Handler:           reg.Handler(logr.FromContextOrDiscard(ctx)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Track images available in containerd and publish via p2p router
	go c.startStateTracker(ctx, ociStore)

	mRouter, err := c.Router(ctx, nodeConfig)
	if err != nil {
		return err
	}
	mRouter.PathPrefix("/v2").Handler(regSvr.Handler)
	mRouter.PathPrefix("/v1-" + version.Program + "/p2p").Handler(c.peerInfo())

	// Wait up to 5 seconds for the p2p network to find peers.
	if err := wait.PollUntilContextTimeout(ctx, time.Second, resolveTimeout, true, func(ctx context.Context) (bool, error) {
		ready, _ := c.router.Ready(ctx)
		return ready, nil
	}); err != nil {
		logrus.Warn("Failed to wait for distributed registry to become ready, will retry in the background")
	}

	return nil
}

func (c *Config) Ready(ctx context.Context) (bool, error) {
	if c.router == nil {
		return false, nil
	}
	return c.router.Ready(ctx)
}

// peerInfo sends a peer address retrieved from the bootstrapper via HTTP
func (c *Config) peerInfo() http.HandlerFunc {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		info, err := c.Bootstrapper.Get(req.Context())
		if err != nil {
			http.Error(resp, err.Error(), http.StatusInternalServerError)
			return
		}

		var addrs []string
		for _, ai := range info {
			for _, ma := range ai.Addrs {
				addrs = append(addrs, fmt.Sprintf("%s/p2p/%s", ma, ai.ID))
			}
		}

		if len(addrs) == 0 {
			http.Error(resp, "no peer addresses available", http.StatusServiceUnavailable)
			return
		}

		client, _, _ := net.SplitHostPort(req.RemoteAddr)
		if req.Header.Get("Accept") == "application/json" {
			b, err := json.Marshal(addrs)
			if err != nil {
				http.Error(resp, err.Error(), http.StatusInternalServerError)
				return
			}
			logrus.Debugf("Serving p2p peer addrs %v to client at %s", addrs, client)
			resp.Header().Set("Content-Type", "application/json")
			resp.WriteHeader(http.StatusOK)
			resp.Write(b)
			return
		}

		logrus.Debugf("Serving p2p peer addr %v to client at %s", addrs[0], client)
		resp.Header().Set("Content-Type", "text/plain")
		resp.WriteHeader(http.StatusOK)
		resp.Write([]byte(addrs[0]))
	})
}
