package tandem

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// withTemporaryNode starts a standalone rqlite on an empty directory, runs fn
// against it, and stops it.
//
// rqlite's /db/load requires a node with no existing data and no other write
// traffic. The supervised pair cannot be used: it is either already serving
// the cluster's live data — which a load would replace in its entirety — or it
// has not started yet, which is exactly when a restore runs. A throwaway
// single member on the freshly emptied directory is the only node shape that
// satisfies the load contract.
//
// The node is a lone member by construction: it has no peer set and no join
// address, so it never contacts the cluster and the cluster never learns of
// it. Loading into a running cluster is what this exists to avoid, and a lone
// member on loopback is also the only shape allowed to run without Raft
// credentials.
func withTemporaryNode(ctx context.Context, rqliteBinary, dataDir string, fn func(base string) error) error {
	binary := resolveBinary("rqlited", "K8E_RQLITED_BINARY")
	if rqliteBinary != "" {
		binary = rqliteBinary
	}
	httpAddr, raftAddr, err := freeLoopbackPair()
	if err != nil {
		return fmt.Errorf("tandem restore: %w", err)
	}
	args := []string{
		"-node-id", "restore",
		"-http-addr", httpAddr,
		"-raft-addr", raftAddr,
		dataDir,
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("tandem restore: start temporary node: %w", err)
	}
	// The temporary node is stopped however fn returns. A load that failed
	// halfway still leaves a process holding the directory the supervised pair
	// is about to open.
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	// The temporary node is a lone member on loopback with no TLS credentials;
	// rqlite requires a plaintext HTTP address for the load endpoint.
	base := "http://" + httpAddr // NOSONAR
	if err := waitHealthy(ctx, base+rqliteReadyzPath, 30*time.Second); err != nil {
		stop()
		return fmt.Errorf("tandem restore: temporary node did not become ready: %w", err)
	}
	loadErr := fn(base)
	stop()
	return loadErr
}

// freeLoopbackPair returns two unused loopback addresses for a temporary
// node's HTTP and Raft listeners. The two listeners must not share a port,
// and both must be free at the same moment, otherwise the node fails to start
// and the restore reports a bind error instead of a loaded store.
func freeLoopbackPair() (http, raft string, err error) {
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
	}
	defer httpListener.Close()
	raftListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
	}
	defer raftListener.Close()
	return httpListener.Addr().String(), raftListener.Addr().String(), nil
}
