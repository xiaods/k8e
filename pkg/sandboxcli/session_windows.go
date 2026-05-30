//go:build windows

package sandboxcli

import "os"

// Windows: no-op lock — state file races acceptable on this platform.
func lockFile(f *os.File) error   { return nil }
func unlockFile(f *os.File) error { return nil }
