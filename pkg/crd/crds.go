package crd

import (
	"github.com/rancher/wrangler/pkg/crd"
	v1 "github.com/xiaods/k8e/pkg/apis/k8e.cattle.io/v1"
)

func List() []crd.CRD {
	addon := crd.NamespacedType("Addon.k8e.cattle.io/v1").
		WithSchemaFromStruct(v1.Addon{}).
		WithColumn("Source", ".spec.source").
		WithColumn("Checksum", ".spec.checksum")

	return []crd.CRD{addon}
}