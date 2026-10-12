package cluster

import (
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/etcd"
	"github.com/xiaods/k8e/pkg/tandem"
)

// init registers the managed datastore drivers in default order. Embedded
// etcd is registered first so it stays the default backend, which is what
// issue #592 requires until the M3 migration gate; Tandem is reachable only
// through an explicit --datastore-backend selection.
func init() {
	managed.RegisterDriver(etcd.NewETCD())
	managed.RegisterDriver(tandem.NewDriver())
}
