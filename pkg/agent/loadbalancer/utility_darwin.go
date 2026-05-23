//go:build darwin
// +build darwin

package loadbalancer

import "syscall"

func reusePort(network, address string, conn syscall.RawConn) error {
	return nil
}
