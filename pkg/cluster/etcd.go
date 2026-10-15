package cluster

import (
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/tandem"
)

// init registers the managed datastore drivers. Tandem (Zig etcd v3
// compat layer over rqlite) is the default and only managed backend;
// embedded etcd is superseded.
func init() {
	managed.RegisterDriver(tandem.NewDriver())
}
