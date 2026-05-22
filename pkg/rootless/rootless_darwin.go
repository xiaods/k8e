//go:build darwin
// +build darwin

package rootless

func Rootless(stateDir string, enableIPv6 bool) error {
	panic("Rootless is not supported on darwin")
}
