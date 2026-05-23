//go:build darwin
// +build darwin

package proctitle

func SetProcTitle(cmd string) {
	// No-op on darwin: macOS does not expose /proc for process title manipulation.
}
