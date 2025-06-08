package cluster

import (
	"context"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/daemons/config"
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

// Start creates the dynamic tls listener, http request handler,
// handles starting and writing/reading bootstrap data, and returns a channel
// that will be closed when datastore is ready. If embedded etcd is in use,
// a secondary call to Cluster.save is made.
func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	// Set up the dynamiclistener and http request handlers
	if err := c.initClusterAndHTTPS(ctx); err != nil {
		return nil, errors.Wrap(err, "init cluster datastore and https")
	}

	if c.config.DisableETCD {
		ready := make(chan struct{})
		defer close(ready)

		// try to get /db/info urls first, for a current list of etcd cluster member client URLs
		clientURLs, _, err := etcd.ClientURLs(ctx, c.clientAccessInfo, c.config.PrivateIP)
		if err != nil {
			return nil, err
		}
		// If we somehow got no error but also no client URLs, just use the address of the server we're joining
		if len(clientURLs) == 0 {
			clientURL, err := url.Parse(c.config.JoinURL)
			if err != nil {
				return nil, err
			}
			clientURL.Host = clientURL.Hostname() + ":2379"
			clientURLs = append(clientURLs, clientURL.String())
			logrus.Warnf("Got empty etcd ClientURL list; using server URL %s", clientURL)
		}
		etcdProxy, err := etcd.NewETCDProxy(ctx, c.config.SupervisorPort, c.config.DataDir, clientURLs[0], utilsnet.IsIPv6CIDR(c.config.ServiceIPRanges[0]))
		if err != nil {
			return nil, err
		}
		// immediately update the load balancer with all etcd addresses
		// client URLs are a full URI, but the proxy only wants host:port
		for i, c := range clientURLs {
			u, err := url.Parse(c)
			if err != nil {
				return nil, errors.Wrap(err, "failed to parse etcd ClientURL")
			}
			clientURLs[i] = u.Host
		}
		etcdProxy.Update(clientURLs)

		// start periodic endpoint sync goroutine
		c.setupEtcdProxy(ctx, etcdProxy)

		// remove etcd member if it exists
		if err := c.managedDB.RemoveSelf(ctx); err != nil {
			logrus.Warnf("Failed to remove this node from etcd members")
		}

		c.config.Runtime.EtcdConfig.Endpoints = strings.Split(c.config.Datastore.Endpoint, ",")
		// Note: TLS configuration should be handled separately as clientv3.Config doesn't have TLSConfig field

		return ready, nil
	}

	// start managed database (if necessary)
	if err := c.start(ctx); err != nil {
		return nil, errors.Wrap(err, "start managed database")
	}

	// get the wait channel for testing managed database readiness
	ready, err := c.testClusterDB(ctx)
	if err != nil {
		return nil, err
	}

	if err := c.startStorage(ctx, false); err != nil {
		return nil, err
	}

	// if necessary, store bootstrap data to datastore
	if c.saveBootstrap {
		if err := Save(ctx, c.config, false); err != nil {
			return nil, err
		}
	}

	// at this point, if etcd is in use, it's bootstrapping is complete
	// so save the bootstrap data. We will need for etcd to be up. If
	// the save call returns an error, we panic since subsequent etcd
	// snapshots will be empty.
	if c.managedDB != nil {
		go func() {
			for {
				select {
				case <-ready:
					if err := Save(ctx, c.config, false); err != nil {
						panic(err)
					}

					if !c.config.EtcdDisableSnapshots {
						_ = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
							err := c.managedDB.ReconcileSnapshotData(ctx)
							if err != nil {
								logrus.Errorf("Failed to record snapshots for cluster: %v", err)
							}
							return err == nil, nil
						})
					}
					return
				default:
					runtime.Gosched()
				}
			}
		}()
	}

	return ready, nil
}

// startStorage configures the native etcd client endpoints and TLS configuration.
// This replaces the previous kine-based storage initialization with direct etcd client setup.
func (c *Cluster) startStorage(ctx context.Context, bootstrap bool) error {
	if c.storageStarted {
		return nil
	}
	c.storageStarted = true

	// Configure etcd endpoints - use embedded etcd by default
	if len(c.config.EtcdEndpoints) == 0 {
		// Default to embedded etcd endpoint
		c.config.EtcdEndpoints = []string{"https://127.0.0.1:2379"}
	}

	// Set up etcd client configuration
	etcdConfig := &clientv3.Config{
		Endpoints:   c.config.EtcdEndpoints,
		DialTimeout: 5 * time.Second,
	}

	if !bootstrap && c.config.EtcdTLSConfig != nil {
		etcdConfig.TLS = c.config.EtcdTLSConfig
	}

	// Store the etcd configuration for use by other components
	c.config.Runtime.EtcdConfig = &clientv3.Config{
		Endpoints:   etcdConfig.Endpoints,
		TLS:         etcdConfig.TLS,
		DialTimeout: etcdConfig.DialTimeout,
	}

	// Enable leader election for multi-node etcd clusters
	c.config.NoLeaderElect = false

	return nil
}

// New creates an initial cluster using the provided configuration.
func New(config *config.Control) *Cluster {
	return &Cluster{
		config: config,
	}
}
