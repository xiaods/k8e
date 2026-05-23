//go:build darwin
// +build darwin

package proctitle

// No-op on darwin: macOS does not expose process title via /proc.
func SetProcTitle(cmd string) {}
