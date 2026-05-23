//go:build darwin
// +build darwin

package cgroups

func Validate() error {
	return nil
}

func CheckCgroups() (kubeletRoot, runtimeRoot string, controllers map[string]bool) {
	return "", "", nil
}
