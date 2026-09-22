package rqlitecompat

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Cluster is a real rqlited cluster started for one test. Tests drive the
// actual binary — there is no mock of the storage engine anywhere in this
// package.
type Cluster struct {
	t               *testing.T
	bin             string
	nodeCount       int
	bootstrapExpect int
	dir             string
	nodes           []*Node
	logs            map[string]string
}

// Node is one rqlited process.
type Node struct {
	ID       string
	HTTPAddr string
	RaftAddr string
	Dir      string
	logPath  string
	cmd      *exec.Cmd
	exited   chan struct{}
}

// Endpoint is the HTTP base URL of the node.
func (n *Node) Endpoint() string {
	// The harness starts rqlited bound to a loopback port of the test machine,
	// so cleartext HTTP never leaves the host.
	return "http://" + n.HTTPAddr // NOSONAR: go:S5332 — loopback-only test process; rqlite's HTTP API has no TLS listener here
}

func (c *Cluster) startNode(n *Node, extraArgs ...string) {
	c.t.Helper()

	logFile, err := os.Create(n.logPath)
	if err != nil {
		c.t.Fatalf("create log for %s: %v", n.ID, err)
	}

	args := []string{
		"-node-id", n.ID,
		"-http-addr", n.HTTPAddr,
		"-raft-addr", n.RaftAddr,
	}
	if c.bootstrapExpect > 1 {
		// Automatic bootstrapping requires the Raft addresses of every node
		// on every node: -bootstrap-expect alone leaves each node
		// bootstrapping itself as a single-node cluster.
		// https://rqlite.io/docs/clustering/automatic-clustering/
		args = append(args,
			"-bootstrap-expect", strconv.Itoa(c.bootstrapExpect),
			"-join", strings.Join(c.raftAddrs(), ","),
		)
	}
	args = append(args, extraArgs...)
	args = append(args, n.Dir)

	cmd := exec.Command(c.bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start %s: %v", n.ID, err)
	}
	n.cmd = cmd
	n.exited = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(n.exited)
		_ = logFile.Close()
	}()
}

// StartCluster boots nodeCount nodes on free loopback ports, waits for every
// node to answer /readyz and for a leader to be known, and registers cleanup.
func StartCluster(t *testing.T, nodeCount int) *Cluster {
	t.Helper()

	bin := requireRQLite(t)
	dir := t.TempDir()

	c := &Cluster{
		t:               t,
		bin:             bin,
		nodeCount:       nodeCount,
		bootstrapExpect: nodeCount,
		dir:             dir,
		logs:            map[string]string{},
	}
	for i := 1; i <= nodeCount; i++ {
		n := &Node{
			ID:       fmt.Sprintf("n%d", i),
			HTTPAddr: hostPort(t),
			RaftAddr: hostPort(t),
			Dir:      filepath.Join(dir, fmt.Sprintf("n%d", i)),
		}
		if err := os.MkdirAll(n.Dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", n.Dir, err)
		}
		n.logPath = filepath.Join(dir, n.ID+".log")
		c.logs[n.ID] = n.logPath
		c.nodes = append(c.nodes, n)
	}

	t.Cleanup(func() {
		c.StopAll()
		if t.Failed() {
			c.DumpLogs()
		}
	})

	for _, n := range c.nodes {
		c.startNode(n)
	}
	c.waitReady()
	c.waitLeader()
	return c
}

// raftAddrs returns every node's Raft address, the form -join expects.
func (c *Cluster) raftAddrs() []string {
	out := make([]string, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, n.RaftAddr)
	}
	return out
}

// Endpoints returns the HTTP endpoints in node order.
func (c *Cluster) Endpoints() []string {
	out := make([]string, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, n.Endpoint())
	}
	return out
}

// Nodes returns the nodes in start order.
func (c *Cluster) Nodes() []*Node { return c.nodes }

// Client returns a client covering every node.
func (c *Cluster) Client() *Client { return NewClient(c.Endpoints()...) }

// NodeByID finds a node by its rqlite node id.
func (c *Cluster) NodeByID(id string) *Node {
	for _, n := range c.nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// Leader returns the node currently reported as Raft leader.
func (c *Cluster) Leader(ctx context.Context) (*Node, error) {
	client := NewClient(c.Endpoints()...)
	var lastErr error
	for _, n := range c.nodes {
		if n.cmd == nil || n.cmd.Process == nil {
			continue
		}
		status, err := client.Status(ctx, n.Endpoint())
		if err != nil {
			lastErr = err
			continue
		}
		if id := status.Store.Leader.NodeID; id != "" {
			if leader := c.NodeByID(id); leader != nil {
				return leader, nil
			}
		}
	}
	return nil, fmt.Errorf("no leader reported by any node: %v", lastErr)
}

// WaitForNewLeader waits until the reported leader differs from oldID.
func (c *Cluster) WaitForNewLeader(ctx context.Context, oldID string) (*Node, error) {
	for {
		leader, err := c.Leader(ctx)
		if err == nil && leader.ID != oldID {
			return leader, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no leader other than %s within timeout (last: %v)", oldID, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Kill sends SIGKILL to a node, simulating an abrupt leader loss.
func (c *Cluster) Kill(n *Node) {
	c.t.Helper()
	if n.cmd == nil || n.cmd.Process == nil {
		c.t.Fatalf("node %s is not running", n.ID)
	}
	if err := n.cmd.Process.Kill(); err != nil {
		c.t.Fatalf("kill %s: %v", n.ID, err)
	}
	select {
	case <-n.exited:
	case <-time.After(10 * time.Second):
		c.t.Fatalf("node %s did not exit after SIGKILL", n.ID)
	}
	n.cmd = nil
}

// Restart starts a stopped node again from its existing data directory.
func (c *Cluster) Restart(n *Node) {
	c.t.Helper()
	c.startNode(n)
}

// StopAll terminates every running node.
func (c *Cluster) StopAll() {
	for _, n := range c.nodes {
		if n.cmd == nil || n.cmd.Process == nil {
			continue
		}
		_ = n.cmd.Process.Signal(os.Interrupt)
	}
	for _, n := range c.nodes {
		if n.cmd == nil {
			continue
		}
		select {
		case <-n.exited:
		case <-time.After(5 * time.Second):
			_ = n.cmd.Process.Kill()
			<-n.exited
		}
		n.cmd = nil
	}
}

// WaitForReady waits until every node answers /readyz and a leader is known.
// It is used after a full cluster restart.
func (c *Cluster) WaitForReady(ctx context.Context) error {
	client := NewClient(c.Endpoints()...)
	report := map[string]error{}
	for {
		ready := 0
		for _, n := range c.nodes {
			if err := client.Ready(ctx, n.Endpoint()); err != nil {
				report[n.ID] = err
				continue
			}
			ready++
		}
		if ready == len(c.nodes) {
			if _, err := c.Leader(ctx); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster not ready within timeout: %v", report)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// DumpLogs prints the tail of every node log. It runs only for a failed test,
// so the evidence of what the node actually did is visible in the test log.
func (c *Cluster) DumpLogs() {
	for _, n := range c.nodes {
		path := c.logs[n.ID]
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}
		fmt.Printf("--- %s log tail (%s) ---\n%s\n", n.ID, path, strings.Join(lines, "\n"))
	}
}

func (c *Cluster) waitReady() {
	c.t.Helper()
	client := NewClient(c.Endpoints()...)
	for _, n := range c.nodes {
		c.waitFor(fmt.Sprintf("%s ready", n.ID), 30*time.Second, func() error {
			return client.Ready(context.Background(), n.Endpoint())
		})
	}
}

func (c *Cluster) waitLeader() {
	c.t.Helper()
	c.waitFor("cluster leader", 30*time.Second, func() error {
		_, err := c.Leader(context.Background())
		return err
	})
}

func (c *Cluster) waitFor(what string, timeout time.Duration, probe func() error) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if lastErr = probe(); lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s: %v", what, lastErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireRQLite returns the rqlited binary path, or skips the test. The M0
// evidence suite is driven by hack/rqlite-m0/run.sh, which downloads and
// sha256-verifies the pinned rqlited and exports RQLITE_BIN; a plain
// `go test ./...` must stay green without it.
func requireRQLite(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RQLITE_BIN")
	if bin == "" {
		t.Skip("RQLITE_BIN not set: run hack/rqlite-m0/run.sh or hack/rqlite-m1/run.sh for the evidence suites")
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("RQLITE_BIN=%q is not usable: %v", bin, err)
	}
	if info.IsDir() {
		t.Fatalf("RQLITE_BIN=%q is a directory, want the rqlited binary", bin)
	}
	return bin
}

func hostPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr.IP.String() + ":" + strconv.Itoa(addr.Port)
}

// waitForNodeCatchUp waits until a restarted node has applied at least index
// in both its Raft log and its SQLite FSM (the two /status fields the driver
// uses to decide a node has caught up).
func (c *Cluster) waitForNodeCatchUp(ctx context.Context, n *Node, index int64) error {
	client := NewClient(n.Endpoint())
	for {
		status, err := client.Status(ctx, n.Endpoint())
		if err == nil && status.Store.DBAppliedIndex >= index && status.Store.Raft.AppliedIndex >= index {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("node %s did not catch up to index %d: %w", n.ID, index, err)
			}
			return fmt.Errorf("node %s applied index db=%d raft=%d, want both >= %d",
				n.ID, status.Store.DBAppliedIndex, status.Store.Raft.AppliedIndex, index)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
