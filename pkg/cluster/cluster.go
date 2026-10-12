package cluster

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"k8s.io/apimachinery/pkg/util/wait"
)

type Cluster struct {
	clientAccessInfo *clientaccess.Info
	config           *config.Control
	managedDB        managed.Driver
	joining          bool
	storageStarted   bool
	// managedStarted guards against starting the managed datastore twice:
	// Bootstrap brings it up when it must read or write bootstrap keys, and
	// Cluster.Start runs afterwards on the same Cluster.
	managedStarted  bool
	saveBootstrap   bool
	shouldBootstrap bool
	cnFilterFunc    func(...string) []string
}

// Start creates the dynamic tls listener, http request handler,
// handles starting and writing/reading bootstrap data, and returns a channel
// that will be closed when the Tandem datastore is ready.
func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	// Set up the dynamiclistener and http request handlers
	if err := c.initClusterAndHTTPS(ctx); err != nil {
		return nil, errors.Wrap(err, "init cluster datastore and https")
	}

	// Tandem is the only supported managed datastore.
	if err := c.start(ctx); err != nil {
		return nil, errors.Wrap(err, "start managed database")
	}

	// get the wait channel for testing managed database readiness
	ready, err := c.testClusterDB(ctx)
	if err != nil {
		return nil, err
	}

	if err := c.startStorage(ctx); err != nil {
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
							if err == nil {
								return true, nil
							}
							// A driver with nothing to reconcile is not
							// failing, and retrying it only produces a
							// permanent log flood — the Tandem store has no
							// snapshot CRs to write, so this ran once a
							// second for the life of the process.
							if errors.Is(err, managed.ErrNotImplemented) {
								logrus.Infof("Driver %s does not maintain snapshot records; nothing to reconcile", c.managedDB.EndpointName())
								return true, nil
							}
							logrus.Errorf("Failed to record snapshots for cluster: %v", err)
							return false, nil
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

// startStorage configures the Kubernetes storage client to use Tandem's etcd-compatible endpoint.
// It populates the runtime EtcdConfig with the endpoints and TLS configuration
// derived from the datastore configuration.
func (c *Cluster) startStorage(ctx context.Context) error {
	if c.storageStarted {
		return nil
	}
	c.storageStarted = true

	// The datastore endpoint serves mTLS now that the driver requires client
	// certificates, so the storage client is configured with K8E's etcd
	// credentials on every path, including bootstrap — which is the first
	// path to reach the datastore. An operator pointing at an external
	// datastore supplies their own credentials via the datastore TLS flags,
	// and those take precedence.
	if c.config.Datastore.BackendTLSConfig.CAFile == "" {
		c.config.Datastore.ServerTLSConfig.CAFile = c.config.Runtime.ETCDServerCA
		c.config.Datastore.ServerTLSConfig.CertFile = c.config.Runtime.ServerETCDCert
		c.config.Datastore.ServerTLSConfig.KeyFile = c.config.Runtime.ServerETCDKey
		c.config.Datastore.BackendTLSConfig = c.config.Datastore.ServerTLSConfig
	}

	// Direct etcd endpoint configuration — no kine intermediary
	endpoints := strings.Split(c.config.Datastore.Endpoint, ",")
	c.config.Runtime.EtcdConfig = config.ETCDConfig{
		Endpoints:   endpoints,
		TLSConfig:   c.config.Datastore.BackendTLSConfig,
		LeaderElect: true,
	}

	c.config.Datastore.BackendTLSConfig = c.config.Runtime.EtcdConfig.TLSConfig
	c.config.Datastore.Endpoint = strings.Join(c.config.Runtime.EtcdConfig.Endpoints, ",")
	c.config.NoLeaderElect = false

	return nil
}

// New creates an initial cluster using the provided configuration.
func New(config *config.Control) *Cluster {
	return &Cluster{
		config: config,
	}
}
