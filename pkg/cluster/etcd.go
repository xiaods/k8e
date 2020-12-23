// +build !no_etcd

package cluster

import (
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/etcd"
)

func init() {
	managed.RegisterDriver(etcd.NewETCD())
}
