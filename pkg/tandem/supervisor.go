// Package tandem manages the local Tandem protocol layer and its rqlite
// storage process. k8e owns both lifecycles; neither child is expected to be
// started independently in a managed installation.
package tandem

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type Config struct {
	DataDir       string
	RqliteBinary  string
	TandemBinary  string
	RqliteHTTP    string
	RqliteRaft    string
	RqliteJoin    string
	AdvertiseIP   string
	TandemListen  string
	TandemTLSCert string
	TandemTLSKey  string
	TandemTLSCA   string
	TandemMTLS    bool
	NodeID        string
	HealthTimeout time.Duration
	Stdout        io.Writer
	Stderr        io.Writer
}

type Supervisor struct {
	cfg    Config
	rqlite *exec.Cmd
	tandem *exec.Cmd

	// mu guards the child commands and the lifecycle fields below, which the
	// monitor goroutine mutates while callers read readiness and shut down.
	mu sync.Mutex
	// runCtx is the context the supervised children and the monitor goroutine
	// run under. It is intentionally a field rather than a parameter: it is
	// derived from the context passed to Start but must outlive that call, so
	// Close can cancel a monitor that started long after Start returned. It is
	// detached from the caller's cancellation on purpose — a caller's deadline
	// ending must not tear down a datastore the supervisor was asked to keep
	// running.
	runCtx         context.Context // NOSONAR godre:S8242 - must outlive Start; see above
	cancel         context.CancelFunc
	monitor        sync.WaitGroup
	monitorStarted bool
	closing        bool
	restarts       int
}

func withDefaults(c Config) Config {
	if c.DataDir == "" {
		c.DataDir = "/var/lib/k8e/tandem"
	}
	if c.RqliteBinary == "" {
		c.RqliteBinary = resolveBinary("rqlited", "K8E_RQLITED_BINARY")
	}
	if c.TandemBinary == "" {
		c.TandemBinary = resolveBinary("tandem", "K8E_TANDEM_BINARY")
	}
	if c.RqliteHTTP == "" {
		c.RqliteHTTP = "127.0.0.1:4001"
	}
	if c.RqliteRaft == "" {
		c.RqliteRaft = "127.0.0.1:4002"
	}
	if c.TandemListen == "" {
		c.TandemListen = "127.0.0.1:2379"
	}
	if c.NodeID == "" {
		c.NodeID, _ = os.Hostname()
		if c.NodeID == "" {
			c.NodeID = "1"
		}
	}
	if c.AdvertiseIP != "" {
		// The SQL API has no authentication configured and must stay local.
		c.RqliteRaft = net.JoinHostPort(c.AdvertiseIP, "4002")
	}
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 30 * time.Second
	}
	return c
}

func (s *Supervisor) Start(ctx context.Context, cfg Config) error {
	s.cfg = withDefaults(cfg)
	rqliteDir := filepath.Join(s.cfg.DataDir, "rqlite")
	if err := prepareDataDir(rqliteDir); err != nil {
		return err
	}

	// The children outlive this call, so they run on a context derived from a
	// fresh cancellable parent rather than the caller's: a caller that cancels
	// its own context after Start returns (or returns at all) must not take the
	// datastore down. Only Close stops the pair.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.runCtx, s.cancel, s.closing, s.restarts = runCtx, cancel, false, 0
	s.mu.Unlock()

	if err := s.startRqlite(runCtx, rqliteDir); err != nil {
		cancel()
		return err
	}
	if err := waitHealthy(ctx, rqliteURL(s.cfg.RqliteHTTP)+rqliteReadyzPath, s.cfg.HealthTimeout); err != nil {
		_ = s.Close()
		return fmt.Errorf("rqlite readiness: %w", err)
	}
	if err := s.startTandem(runCtx); err != nil {
		_ = s.Close()
		return err
	}
	if err := waitTCP(ctx, s.cfg.TandemListen, s.cfg.HealthTimeout); err != nil {
		_ = s.Close()
		return fmt.Errorf("tandem listen readiness: %w", err)
	}

	// Both children are up. From here a crash is recovered rather than fatal:
	// rqlite first (the compat layer cannot serve without it), then the layer.
	s.monitor.Add(1)
	s.mu.Lock()
	s.monitorStarted = true
	s.mu.Unlock()
	go s.supervise(runCtx)
	return nil
}

func (s *Supervisor) startRqlite(ctx context.Context, rqliteDir string) error {
	cmd := exec.CommandContext(ctx, s.cfg.RqliteBinary,
		"-node-id", s.cfg.NodeID,
		"-http-addr", s.cfg.RqliteHTTP,
		"-raft-addr", s.cfg.RqliteRaft,
		"-raft-cluster-remove-shutdown=true",
		rqliteDir,
	)
	if s.cfg.RqliteJoin != "" {
		cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "-join", s.cfg.RqliteJoin, rqliteDir)
	}
	cmd.Stdout, cmd.Stderr = s.cfg.Stdout, s.cfg.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start rqlite: %w", err)
	}
	s.mu.Lock()
	s.rqlite = cmd
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) startTandem(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.cfg.TandemBinary)
	cmd.Env = append(os.Environ(), tandemEnvironment(s.cfg)...)
	cmd.Stdout, cmd.Stderr = s.cfg.Stdout, s.cfg.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tandem: %w", err)
	}
	s.mu.Lock()
	s.tandem = cmd
	s.mu.Unlock()
	return nil
}

// supervise restarts a child that exits on its own. Issue #592 requires crash
// restart to be explicit, so a dead rqlite or a dead compat layer is brought
// back with the same configuration instead of leaving the node silently
// degraded. Both children are watched concurrently: the compat layer failing
// while rqlite stays up is the common case and must not wait on rqlite. The
// backoff bounds how fast a child that cannot start is retried; after
// maxChildRestarts the monitor gives up rather than spin.
func (s *Supervisor) supervise(ctx context.Context) {
	defer s.monitor.Done()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.watch(ctx, "rqlite")
	}()
	go func() {
		defer wg.Done()
		s.watch(ctx, "tandem")
	}()
	wg.Wait()
}

// watch restarts the named child whenever it exits on its own, unless the
// supervisor is shutting down. It keeps watching after a successful restart so
// a later crash is recovered too, and gives up once the restart budget is
// exhausted so a child that cannot start does not spin. It blocks until the
// supervisor context is cancelled or the budget runs out.
func (s *Supervisor) watch(ctx context.Context, name string) {
	for {
		died := s.waitChild(s.childField(name))
		if !s.waitExit(ctx, died) {
			return
		}

		s.mu.Lock()
		stopping := s.closing
		s.restarts++
		exhausted := s.restarts > maxChildRestarts
		s.mu.Unlock()
		if stopping {
			return
		}
		if exhausted {
			logrus.Errorf("tandem: %s exceeded %d restarts; leaving it down", name, maxChildRestarts)
			return
		}

		delay := restartBackoff(s.restarts)
		logrus.Warnf("tandem: %s exited; restarting in %s (attempt %d)", name, delay, s.restarts)
		if !s.sleep(ctx, delay) {
			return
		}

		if err := s.restart(ctx, name); err != nil {
			logrus.Errorf("tandem: restart %s: %v", name, err)
			continue
		}
		logrus.Infof("tandem: %s restarted", name)
	}
}

// childField returns a pointer to the named child's command so waitChild can
// read it under the lock.
func (s *Supervisor) childField(name string) **exec.Cmd {
	if name == "tandem" {
		return &s.tandem
	}
	return &s.rqlite
}

// waitExit reports whether the child died, as opposed to the supervisor being
// told to shut down.
func (s *Supervisor) waitExit(ctx context.Context, died <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-died:
		return true
	}
}

func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// restart brings one child back and waits for it to become serviceable again.
func (s *Supervisor) restart(ctx context.Context, name string) error {
	switch name {
	case "rqlite":
		if err := s.startRqlite(ctx, filepath.Join(s.cfg.DataDir, "rqlite")); err != nil {
			return err
		}
		if err := waitHealthy(ctx, rqliteURL(s.cfg.RqliteHTTP)+rqliteReadyzPath, s.cfg.HealthTimeout); err != nil {
			return fmt.Errorf("rqlite did not become ready: %w", err)
		}
	case "tandem":
		if err := s.startTandem(ctx); err != nil {
			return err
		}
		if err := waitTCP(ctx, s.cfg.TandemListen, s.cfg.HealthTimeout); err != nil {
			return fmt.Errorf("tandem did not listen: %w", err)
		}
	}
	return nil
}

// waitChild reaps the named child and returns a channel closed once it has
// exited. The caller passes the address of the field so the read happens under
// the lock rather than racing the restart that replaces it.
func (s *Supervisor) waitChild(field **exec.Cmd) <-chan struct{} {
	done := make(chan struct{})
	s.mu.Lock()
	cmd := *field
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()
	return done
}

// maxChildRestarts bounds crash recovery so a child that cannot start (a
// missing binary, a corrupt store) does not restart forever.
const maxChildRestarts = 5

// rqlite's readiness endpoint. A node answers it once the store is open and it
// has elected a leader, which is the point at which a read is guaranteed to be
// linearizable rather than merely answered.
const rqliteReadyzPath = "/readyz"

func restartBackoff(attempt int) time.Duration {
	delay := time.Duration(1<<uint(attempt-1)) * time.Second
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

// prepareDataDir refuses to start rqlited against a directory that already
// holds data K8E did not put there. rqlite happily initialises a fresh empty
// store in a non-empty directory, so without this check a corrupt, foreign or
// half-removed data dir would be silently replaced by an empty database and
// the cluster would come up with no objects. Issue #592 requires that reusing
// a data dir never implicitly rebuilds an empty store.
func prepareDataDir(rqliteDir string) error {
	entries, err := os.ReadDir(rqliteDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect rqlite data dir: %w", err)
		}
		return os.MkdirAll(rqliteDir, 0700)
	}
	if len(entries) == 0 {
		return nil
	}
	if !hasRqliteState(rqliteDir) {
		return fmt.Errorf("rqlite data dir %s is not empty and holds no rqlite state; refusing to rebuild an empty database over it", rqliteDir)
	}
	return nil
}

// hasRqliteState reports whether the directory holds a rqlite store rather than
// unrelated files. rqlited writes db.sqlite (plus its -wal/-shm siblings) and
// raft.db beside the raft/ directory it snapshots into.
func hasRqliteState(rqliteDir string) bool {
	for _, name := range []string{"db.sqlite", "db.sqlite-wal", "db.sqlite-shm", "raft.db"} {
		if _, err := os.Stat(filepath.Join(rqliteDir, name)); err == nil {
			return true
		}
	}
	// A node that was snapshotted, or whose store was removed but whose Raft
	// directory survived, still carries rqlite's own layout.
	if info, err := os.Stat(filepath.Join(rqliteDir, "raft")); err == nil && info.IsDir() {
		return true
	}
	if info, err := os.Stat(filepath.Join(rqliteDir, "wsnapshots")); err == nil && info.IsDir() {
		return true
	}
	return false
}

// Ready reports whether rqlite can currently serve the cluster: its /readyz
// answers and a leader is known. rqlite returns 503 from /readyz while it has
// no quorum, so this is what separates "the process is up" from "the datastore
// can serve a request". A single-node cluster is ready as soon as it elects
// itself, which is also what /readyz reports once bootstrap completes.
func (s *Supervisor) Ready(ctx context.Context) error {
	if s.rqlite == nil || s.rqlite.Process == nil {
		return fmt.Errorf("rqlite is not running")
	}
	base := rqliteURL(s.cfg.RqliteHTTP)
	if err := waitHealthy(ctx, base+rqliteReadyzPath, readinessPollTimeout); err != nil {
		return fmt.Errorf("rqlite not ready: %w", err)
	}
	if err := waitLeader(ctx, base+"/status", readinessPollTimeout); err != nil {
		return fmt.Errorf("rqlite has no leader: %w", err)
	}
	return nil
}

// readinessPollTimeout bounds a single Ready call. The caller retries, so this
// only needs to be long enough to absorb a transient leader election.
const readinessPollTimeout = 2 * time.Second

func waitLeader(ctx context.Context, statusURL string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		if leaderKnown(ctx, client, statusURL) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("no leader reported by %s", statusURL)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// leaderKnown reports whether rqlite's /status says the store is ready and
// names a leader. rqlite answers 200 with an empty or not-yet-ready "store"
// object before an election completes, so a 200 alone is not readiness.
func leaderKnown(ctx context.Context, client *http.Client, statusURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, http.NoBody)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var status rqliteStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return false
	}
	return status.Store.Ready && status.Store.Leader.Addr != ""
}

// rqliteStatus is the part of rqlite's /status response this supervisor reads:
// whether the store is ready, and whether it knows a leader. A node can serve a
// socket before it elects itself, which is why readiness checks both.
type rqliteStatus struct {
	Store rqliteStoreStatus `json:"store"`
}

type rqliteStoreStatus struct {
	Ready  bool             `json:"ready"`
	Leader rqliteLeaderInfo `json:"leader"`
}

type rqliteLeaderInfo struct {
	Addr string `json:"addr"`
}

func tandemEnvironment(cfg Config) []string {
	environment := []string{
		"TANDEM_DATA_DIR=" + cfg.DataDir,
		"TANDEM_LISTEN_ADDR=" + cfg.TandemListen,
		"TANDEM_RQLITE_ADDR=http://" + cfg.RqliteHTTP,
		fmt.Sprintf("TANDEM_TLS_REQUIRE_CLIENT_CERT=%t", cfg.TandemMTLS),
	}
	if cfg.TandemTLSCert != "" {
		environment = append(environment, "TANDEM_TLS_CERT_FILE="+cfg.TandemTLSCert)
	}
	if cfg.TandemTLSKey != "" {
		environment = append(environment, "TANDEM_TLS_KEY_FILE="+cfg.TandemTLSKey)
	}
	if cfg.TandemTLSCA != "" {
		environment = append(environment, "TANDEM_TLS_CA_FILE="+cfg.TandemTLSCA)
	}
	return environment
}

// Close stops both children and the monitor. It is safe to call more than
// once and safe to call after a failed Start. The monitor is told we are
// shutting down first so an exit it observes is not mistaken for a crash and
// restarted on the way down.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	cancel := s.cancel
	tandem, rqlite := s.tandem, s.rqlite
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// The monitor owns Wait for each child; if it never started (a failed
	// Start), reap here so no zombie is left behind.
	s.mu.Lock()
	supervised := s.runCtx != nil && s.monitorStarted
	s.mu.Unlock()
	if !supervised {
		stopChild(tandem)
		stopChild(rqlite)
	}
	s.monitor.Wait()
	return nil
}

// stopChild asks a child to exit and reaps it.
func stopChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	_ = cmd.Wait()
}

// rqliteURL builds the loopback base URL for a supervised rqlite node.
//
// The scheme is fixed rather than taken from configuration because rqlite is
// always a child process on this host: it listens on 127.0.0.1 and the request
// never leaves the machine, so there is no plaintext to protect on this hop.
// The node's own authentication is irrelevant here — it answers a health probe
// before serving anything, and the datastore is reached through Tandem's mTLS
// etcd port, not this one.
//
// NOSONAR: go:S5332 — see above; the alternative is an unused TLS listener
// with no certificate authority to validate against.
func rqliteURL(host string) string {
	return "http://" + host // NOSONAR
}

func waitHealthy(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		resp, err := client.Get(endpoint)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for %s", endpoint)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	target := dialAddress(addr)
	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", target)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for %s: %w", target, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func dialAddress(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listenAddr
}

func resolveBinary(name, envVar string) string {
	if env := os.Getenv(envVar); env != "" {
		if _, err := exec.LookPath(env); err == nil {
			return env
		}
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	for _, dir := range []string{"/usr/local/bin", "/opt/k8e/bin", "bin"} {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return name
}
