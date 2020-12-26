package cluster

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"github.com/rancher/kine/pkg/client"
	"github.com/rancher/kine/pkg/endpoint"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

type Cluster struct {
	clientAccessInfo *clientaccess.Info
	config           *config.Control
	runtime          *config.ControlRuntime
	managedDB        managed.Driver
	shouldBootstrap  bool
	storageStarted   bool
	etcdConfig       endpoint.ETCDConfig
	joining          bool
	saveBootstrap    bool
	storageClient    client.Client
}

// Start creates the dynamic tls listener, http request handler,
// handles starting and writing/reading bootstrap data, and returns a channel
// that will be closed when datastore is ready.
func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	// Set up the dynamiclistener and http request handlers
	if err := c.initClusterAndHTTPS(ctx); err != nil {
		return nil, errors.Wrap(err, "init cluster datastore and https")
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

	// if necessary, store bootstrap data to datastore
	if c.saveBootstrap {
		if err := c.save(ctx); err != nil {
			return nil, err
		}
	}

	// if necessary, record successful bootstrap
	if c.shouldBootstrap {
		if err := c.bootstrapped(); err != nil {
			return nil, err
		}
	}

	return ready, c.startStorage(ctx)
}

// startStorage starts the kine listener and configures the endpoints, if necessary.
// This calls into the kine endpoint code, which sets up the database client
// and unix domain socket listener if using an external database. In the case of an etcd
// backend it just returns the user-provided etcd endpoints and tls config.
func (c *Cluster) startStorage(ctx context.Context) error {
	if c.storageStarted {
		return nil
	}
	c.storageStarted = true

	// start listening on the kine socket as an etcd endpoint, or return the external etcd endpoints
	etcdConfig, err := endpoint.Listen(ctx, c.config.Datastore)
	if err != nil {
		return errors.Wrap(err, "creating storage endpoint")
	}

	// Persist the returned etcd configuration. We decide if we're doing leader election for embedded controllers
	// based on what the kine wrapper tells us about the datastore. Single-node datastores like sqlite don't require
	// leader election, while basically all others (etcd, external database, etc) do since they allow multiple servers.
	c.etcdConfig = etcdConfig
	c.config.Datastore.Config = etcdConfig.TLSConfig
	c.config.Datastore.Endpoint = strings.Join(etcdConfig.Endpoints, ",")
	c.config.NoLeaderElect = !etcdConfig.LeaderElect
	return nil
}

// New creates an initial cluster using the provided configuration
func New(config *config.Control) *Cluster {
	return &Cluster{
		config:  config,
		runtime: config.Runtime,
	}
}
