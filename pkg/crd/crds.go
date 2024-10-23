package crd

import (
	"github.com/rancher/wrangler/v3/pkg/crd"
	v1 "github.com/xiaods/k8e/pkg/apis/k8e.cattle.io/v1"
)

func List() []crd.CRD {
	addon := v1.Addon{}
	etcdSnapshotFile := v1.ETCDSnapshotFile{}
	return []crd.CRD{
		crd.NamespacedType("Addon.k3s.cattle.io/v1").
			WithSchemaFromStruct(addon).
			WithColumn("Source", ".spec.source").
			WithColumn("Checksum", ".spec.checksum"),
		crd.NonNamespacedType("ETCDSnapshotFile.k3s.cattle.io/v1").
			WithSchemaFromStruct(etcdSnapshotFile).
			WithColumn("SnapshotName", ".spec.snapshotName").
			WithColumn("Node", ".spec.nodeName").
			WithColumn("Location", ".spec.location").
			WithColumn("Size", ".status.size").
			WithColumn("CreationTime", ".status.creationTime"),
	}
}
