package crd

import (
	"github.com/rancher/wrangler/v3/pkg/crd"
	v1 "github.com/xiaods/k8e/pkg/apis/k8e.cattle.io/v1"
	v1alpha1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

func List() []crd.CRD {
	addon := v1.Addon{}
	etcdSnapshotFile := v1.ETCDSnapshotFile{}
	return []crd.CRD{
		crd.NamespacedType("Addon.k8e.cattle.io/v1").
			WithSchemaFromStruct(addon).
			WithColumn("Source", ".spec.source").
			WithColumn("Checksum", ".spec.checksum"),
		crd.NonNamespacedType("ETCDSnapshotFile.k8e.cattle.io/v1").
			WithSchemaFromStruct(etcdSnapshotFile).
			WithColumn("SnapshotName", ".spec.snapshotName").
			WithColumn("Node", ".spec.nodeName").
			WithColumn("Location", ".spec.location").
			WithColumn("Size", ".status.size").
			WithColumn("CreationTime", ".status.creationTime"),
		crd.NamespacedType("SandboxMatrix.k8e.cattle.io/v1alpha1").
			WithSchemaFromStruct(v1alpha1.SandboxMatrix{}).
			WithColumn("WarmPool", ".spec.warmPoolSize").
			WithColumn("Runtime", ".spec.runtimeClass").
			WithColumn("Active", ".status.activeSessions"),
		crd.NamespacedType("SandboxSession.k8e.cattle.io/v1alpha1").
			WithSchemaFromStruct(v1alpha1.SandboxSession{}).
			WithColumn("Phase", ".status.phase").
			WithColumn("Pod", ".status.podName").
			WithColumn("Runtime", ".spec.runtimeClass"),
		crd.NamespacedType("SandboxWarmPool.k8e.cattle.io/v1alpha1").
			WithSchemaFromStruct(v1alpha1.SandboxWarmPool{}).
			WithColumn("Size", ".spec.size").
			WithColumn("Ready", ".status.readyCount"),
		crd.NamespacedType("SandboxTemplate.k8e.cattle.io/v1alpha1").
			WithSchemaFromStruct(v1alpha1.SandboxTemplate{}).
			WithColumn("Runtime", ".spec.runtimeClass").
			WithColumn("Image", ".spec.image"),
	}
}
