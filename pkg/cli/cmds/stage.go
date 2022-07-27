//go:build !no_stage
// +build !no_stage

package cmds

const (
	// coredns run controllers that are turned off when their manifests are disabled.
	// The k8e CloudController also has a bundled manifest and can be disabled via the
	// --disable-cloud-controller flag or --disable=ccm, but the latter method is not documented.
	DisableItems = "coredns, local-storage, metrics-server"
)
