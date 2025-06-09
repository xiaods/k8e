package cluster

import (
	"context"
	"crypto/tls"
	"net/url"
	"strings"
	"time"

	pkgerrors "github.com/pkg/errors"
	"github.com/rancher/dynamiclistener/cert"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/apimachinery/pkg/util/wait"
	utilsnet "k8s.io/utils/net"
)

type Cluster struct {
	clientAccessInfo *clientaccess.Info
	config           *config.Control
	managedDB        managed.Driver
	joining          bool
	storageStarted   bool
	saveBootstrap    bool
	shouldBootstrap  bool
	cnFilterFunc     func(...string) []string
}

// ListenAndServe creates the dynamic tls listener, registers http request
// handlers, and starts the supervisor API server loop.
func (c *Cluster) ListenAndServe(ctx context.Context) error {
	// Set up the dynamiclistener and http request handlers
	return c.initClusterAndHTTPS(ctx)
}

// Start handles writing/reading bootstrap data. If embedded etcd is in use,
// a secondary call to Cluster.save is made.
func (c *Cluster) Start(ctx context.Context) error {
	if c.config.DisableETCD || c.managedDB == nil {
		// if etcd is disabled or we're using kine, perform a no-op start of etcd
		// to close the etcd ready channel. When etcd is in use, this is handled by
		// c.start() -> c.managedDB.Start() -> etcd.Start() -> executor.ETCD()
		executor.ETCD(ctx, nil, nil, func(context.Context) error { return nil })
	}

	if c.config.DisableETCD {
		return nil
	}

	// start managed etcd database; when kine is in use this is a no-op.
	if err := c.start(ctx); err != nil {
		return pkgerrors.WithMessage(err, "start managed database")
	}

	// set c.config.Datastore and c.config.Runtime.EtcdConfig with values
	// necessary to build etcd clients, and start kine listener if necessary.
	if err := c.startStorage(ctx, false); err != nil {
		return err
	}

	// if necessary, store bootstrap data to datastore. saveBootstrap is only set
	// when using kine, so this can be done before the ready channel has been closed.
	if c.saveBootstrap {
		if err := Save(ctx, c.config, false); err != nil {
			return err
		}
	}

	if c.managedDB != nil {
		go func() {
			for {
				select {
				case <-executor.ETCDReadyChan():
					// always save to managed etcd, to ensure that any file modified locally are in sync with the datastore.
					// this will panic if multiple keys exist, to prevent nodes from running with different bootstrap data.
					if err := Save(ctx, c.config, false); err != nil {
						panic(err)
					}

					if !c.config.EtcdDisableSnapshots {
						// do an initial reconcile of snapshots with a fast retry until it succeeds
						wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
							if err := c.managedDB.ReconcileSnapshotData(ctx); err != nil {
								logrus.Errorf("Failed to record snapshots for cluster: %v", err)
								return false, nil
							}
							return true, nil
						})

						// continue reconciling snapshots in the background at the configured interval.
						// the interval is jittered by 5% to avoid all nodes reconciling at the same time.
						wait.JitterUntilWithContext(ctx, func(ctx context.Context) {
							if err := c.managedDB.ReconcileSnapshotData(ctx); err != nil {
								logrus.Errorf("Failed to record snapshots for cluster: %v", err)
							}
						}, c.config.EtcdSnapshotReconcile.Duration, 0.05, false)
					}
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	return nil
}

// startEtcdProxy starts an etcd load-balancer proxy, for control-plane-only nodes
// without a local datastore.
func (c *Cluster) startEtcdProxy(ctx context.Context) error {
	defaultURL, err := url.Parse(c.config.JoinURL)
	if err != nil {
		return err
	}
	defaultURL.Host = defaultURL.Hostname() + ":2379"
	etcdProxy, err := etcd.NewETCDProxy(ctx, c.config.SupervisorPort, c.config.DataDir, defaultURL.String(), utilsnet.IsIPv6CIDR(c.config.ServiceIPRanges[0]))
	if err != nil {
		return err
	}

	// immediately update the load balancer with all etcd addresses
	// from /db/info, for a current list of etcd cluster member client URLs.
	// client URLs are a full URI, but the proxy only wants host:port
	if clientURLs, _, err := etcd.ClientURLs(ctx, c.clientAccessInfo, c.config.PrivateIP); err != nil || len(clientURLs) == 0 {
		logrus.Warnf("Failed to get etcd ClientURLs: %v", err)
	} else {
		for i, c := range clientURLs {
			u, err := url.Parse(c)
			if err != nil {
				return pkgerrors.WithMessage(err, "failed to parse etcd ClientURL")
			}
			clientURLs[i] = u.Host
		}
		etcdProxy.Update(clientURLs)
	}

	// start periodic endpoint sync goroutine
	c.setupEtcdProxy(ctx, etcdProxy)

	// remove etcd member if it exists
	if err := c.managedDB.RemoveSelf(ctx); err != nil {
		logrus.Warnf("Failed to remove this node from etcd members: %v", err)
	}

	c.config.Runtime.EtcdConfig.Endpoints = strings.Split(c.config.Datastore.Endpoint, ",")
	// Note: clientv3.Config uses TLS field, not TLSConfig
	if tlsConfig, err := c.createTLSConfigFromBackend(); err == nil {
		c.config.Runtime.EtcdConfig.TLS = tlsConfig
	}

	return nil
}

// startStorage starts the kine listener and configures the endpoints, if necessary.
// This calls into the kine endpoint code, which sets up the database client
// and unix domain socket listener if using an external database. In the case of an etcd
// backend it just returns the user-provided etcd endpoints and tls config.
func (c *Cluster) startStorage(ctx context.Context, bootstrap bool) error {
	if c.storageStarted {
		return nil
	}
	c.storageStarted = true

	if !bootstrap {
		// set the tls config for the kine storage
		c.config.Datastore.ServerTLSConfig.CAFile = c.config.Runtime.ETCDServerCA
		c.config.Datastore.ServerTLSConfig.CertFile = c.config.Runtime.ServerETCDCert
		c.config.Datastore.ServerTLSConfig.KeyFile = c.config.Runtime.ServerETCDKey
	}

	// create native etcd client configuration
	etcdConfig, err := c.createEtcdConfig(ctx)
	if err != nil {
		return pkgerrors.WithMessage(err, "creating etcd configuration")
	}

	// Persist the etcd configuration. We decide if we're doing leader election for embedded controllers
	// based on the datastore configuration. External etcd requires leader election.
	c.config.Runtime.EtcdConfig = etcdConfig

	// after the bootstrap we need to set the args for api-server
	if !bootstrap {
		// For external etcd, always enable leader election
		c.config.NoLeaderElect = false
	}

	return nil
}

// New creates an initial cluster using the provided configuration.
func New(config *config.Control) *Cluster {
	return &Cluster{
		config: config,
	}
}

// createEtcdConfig creates a native etcd client configuration from datastore settings
func (c *Cluster) createEtcdConfig(ctx context.Context) (*clientv3.Config, error) {
	// Parse endpoints from datastore configuration
	endpoints := strings.Split(c.config.Datastore.Endpoint, ",")
	for i, endpoint := range endpoints {
		endpoints[i] = strings.TrimSpace(endpoint)
	}

	// Create base etcd client configuration
	etcdConfig := &clientv3.Config{
		Endpoints:            endpoints,
		Context:              ctx,
		DialTimeout:          5 * time.Second,
		DialKeepAliveTime:    30 * time.Second,
		DialKeepAliveTimeout: 5 * time.Second,
		PermitWithoutStream:  true,
	}

	// Configure TLS if using HTTPS endpoints
	if len(endpoints) > 0 && strings.HasPrefix(endpoints[0], "https://") {
		tlsConfig, err := c.createTLSConfig()
		if err != nil {
			return nil, pkgerrors.WithMessage(err, "creating TLS configuration")
		}
		etcdConfig.TLS = tlsConfig
	}

	return etcdConfig, nil
}

// createTLSConfig creates TLS configuration from datastore backend TLS settings
func (c *Cluster) createTLSConfig() (*tls.Config, error) {
	backendTLS := c.config.Datastore.BackendTLSConfig

	// Check if TLS configuration is available
	if backendTLS.CAFile == "" || backendTLS.CertFile == "" || backendTLS.KeyFile == "" {
		return nil, pkgerrors.New("incomplete TLS configuration: missing CA, cert, or key file")
	}

	// Load client certificate
	clientCert, err := tls.LoadX509KeyPair(backendTLS.CertFile, backendTLS.KeyFile)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "loading client certificate")
	}

	// Load CA certificate pool
	pool, err := cert.NewPool(backendTLS.CAFile)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "loading CA certificate")
	}

	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
	}, nil
}

// createTLSConfigFromBackend creates TLS configuration from datastore backend TLS settings
func (c *Cluster) createTLSConfigFromBackend() (*tls.Config, error) {
	backendTLS := c.config.Datastore.BackendTLSConfig

	// Check if TLS configuration is available
	if backendTLS.CAFile == "" || backendTLS.CertFile == "" || backendTLS.KeyFile == "" {
		return nil, pkgerrors.New("incomplete TLS configuration: missing CA, cert, or key file")
	}

	// Load client certificate
	clientCert, err := tls.LoadX509KeyPair(backendTLS.CertFile, backendTLS.KeyFile)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "loading client certificate")
	}

	// Load CA certificate pool
	pool, err := cert.NewPool(backendTLS.CAFile)
	if err != nil {
		return nil, pkgerrors.WithMessage(err, "loading CA certificate")
	}

	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
	}, nil
}
