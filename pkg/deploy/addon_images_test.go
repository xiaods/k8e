package deploy

import "testing"

// k3s v1.37.0+k3s1 addon image tags. Bindata is generated from manifests/,
// so these strings are the staged runtime images.
func TestAddonImagesTrackK3s137(t *testing.T) {
	coredns := assetBytes(t, "coredns.yaml")
	assertContains(t, coredns, "rancher/mirrored-coredns-coredns:1.14.7", "coredns.yaml")
	assertNoneContain(t, coredns, "coredns.yaml", []string{"coredns-coredns:1.10.1"})

	metrics := assetBytes(t, "metrics-server/metrics-server-deployment.yaml")
	assertContains(t, metrics, "rancher/mirrored-metrics-server:v0.9.0", "metrics-server-deployment.yaml")
	assertNoneContain(t, metrics, "metrics-server-deployment.yaml", []string{"metrics-server:v0.8.1"})

	local := assetBytes(t, "local-storage.yaml")
	assertContains(t, local, "rancher/local-path-provisioner:v0.0.37", "local-storage.yaml")
	assertContains(t, local, "rancher/mirrored-library-busybox:1.37.0", "local-storage.yaml")
	assertContains(t, local, `"${VOL_DIR}"`, "local-storage.yaml")
	assertNoneContain(t, local, "local-storage.yaml", []string{
		"local-path-provisioner:v0.0.30",
		"busybox:1.36.1",
	})
}
