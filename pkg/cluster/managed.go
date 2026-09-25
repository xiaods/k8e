package cluster

// A managed database is one whose lifecycle we control. Tandem is the only
// supported managed datastore and owns the rqlite-backed cluster lifecycle.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/nodepassword"
	"github.com/xiaods/k8e/pkg/version"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// testClusterDB returns a channel that will be closed when the datastore connection is available.
// The datastore is tested for readiness every 5 seconds until the test succeeds.
func (c *Cluster) testClusterDB(ctx context.Context) (<-chan struct{}, error) {
	result := make(chan struct{})
	if c.managedDB == nil {
		close(result)
		return result, nil
	}

	go func() {
		defer close(result)
		for {
			if err := c.managedDB.Test(ctx); err != nil {
				logrus.Infof("Failed to test data store connection: %v", err)
			} else {
				logrus.Info(c.managedDB.EndpointName() + " data store connection OK")
				return
			}

			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()

	return result, nil
}

// start starts the database, unless a cluster reset has been requested, in which case
// it does that instead.
// managedStarted records that the managed datastore has already been brought
// up. Bootstrap starts the driver when it needs to read or write bootstrap
// keys, which happens before Cluster.Start; without this the second call would
// start a second copy of the datastore.
func (c *Cluster) start(ctx context.Context) error {
	if c.managedDB == nil {
		return nil
	}
	rebootstrap := func() error {
		return c.storageBootstrap(ctx)
	}

	resetDone, err := c.managedDB.IsReset()
	if err != nil {
		return err
	}

	if c.config.ClusterReset {
		// If we're restoring from a snapshot, don't check the reset-flag - just reset and restore.
		if c.config.ClusterResetRestorePath != "" {
			return c.managedDB.Reset(ctx, rebootstrap)
		}

		// If the reset-flag doesn't exist, reset. This will create the reset-flag if it succeeds.
		if !resetDone {
			return c.managedDB.Reset(ctx, rebootstrap)
		}

		// The reset-flag exists, ask the user to remove it if they want to reset again.
		// The driver names itself: the default backend is still embedded etcd,
		// but telling an operator "Managed etcd" while they are running the
		// Tandem backend sends them to the wrong data directory.
		return fmt.Errorf("Managed %s cluster membership was previously reset, please remove the cluster-reset flag and start %s normally. "+
			"If you need to perform another cluster reset, you must first manually delete the file at %s", c.managedDB.EndpointName(), version.Program, c.managedDB.ResetFile())
	}

	if resetDone {
		// If the cluster was reset, we need to delete the node passwd secret in case the node
		// password from the previously restored snapshot differs from the current password on disk.
		c.config.Runtime.ClusterControllerStarts["node-password-secret-cleanup"] = c.deleteNodePasswdSecret
	}

	// Starting the managed database will clear the reset-flag if set
	if c.managedStarted {
		return nil
	}
	if err := c.managedDB.Start(ctx, c.clientAccessInfo); err != nil {
		return err
	}
	c.managedStarted = true
	return nil
}

// registerDBHandlers registers managed-datastore-specific callbacks, and installs additional HTTP route handlers.
// Note that for etcd, controllers only run on nodes with a local apiserver, in order to provide stable external
// management of etcd cluster membership without being disrupted when a member is removed from the cluster.
func (c *Cluster) registerDBHandlers(handler http.Handler) (http.Handler, error) {
	if c.managedDB == nil {
		return handler, nil
	}

	return c.managedDB.Register(handler)
}

// assignManagedDriver selects the managed datastore driver for this server.
//
// Issue #592 requires that embedded etcd stays the default backend until the
// M3 migration gate, and that a new backend is only reachable by explicitly
// opting in. Selection therefore prefers whichever driver already has an
// initialized data dir on disk, so an existing cluster keeps its backend across
// upgrades, and otherwise picks a driver only when the operator asked for a
// managed datastore.
func (c *Cluster) assignManagedDriver(ctx context.Context) error {
	// A driver with an initialized data dir on disk owns this cluster already.
	for _, driver := range managed.Registered() {
		if err := driver.SetControlConfig(c.config); err != nil {
			return err
		}
		initialized, err := driver.IsInitialized()
		if err != nil {
			return err
		}
		if initialized {
			c.managedDB = driver
			return nil
		}
	}

	// An explicitly configured endpoint means the operator is pointing at a
	// datastore they manage; K8E starts no local one.
	if c.config.Datastore.Endpoint != "" && c.config.Datastore.Backend == "" {
		return nil
	}

	selected, err := managed.Select(c.config.Datastore.Backend)
	if err != nil {
		return err
	}
	if selected == nil {
		return nil
	}
	if err := selected.SetControlConfig(c.config); err != nil {
		return err
	}
	c.managedDB = selected
	return nil
}

// deleteNodePasswdSecret wipes out the node password secret after restoration
func (c *Cluster) deleteNodePasswdSecret(ctx context.Context) {
	nodeName := os.Getenv("NODE_NAME")
	secretsClient := c.config.Runtime.Core.Core().V1().Secret()
	if err := nodepassword.Delete(secretsClient, nodeName); err != nil {
		if apierrors.IsNotFound(err) {
			logrus.Debugf("Node password secret is not found for node %s", nodeName)
			return
		}
		logrus.Warnf("failed to delete old node password secret: %v", err)
	}
}
