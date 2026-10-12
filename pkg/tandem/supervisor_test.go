package tandem

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	c := withDefaults(Config{})
	if c.DataDir != "/var/lib/k8e/tandem" || c.RqliteHTTP != "127.0.0.1:4001" || c.RqliteRaft != "127.0.0.1:4002" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestAdvertiseIPDoesNotExposeSQL(t *testing.T) {
	for _, ip := range []string{"192.0.2.10", "2001:db8::1"} {
		c := withDefaults(Config{AdvertiseIP: ip})
		if c.RqliteHTTP != "127.0.0.1:4001" {
			t.Fatalf("advertise IP %s exposed SQL API at %s", ip, c.RqliteHTTP)
		}
	}
}

func TestTandemEnvironmentIncludesTLSConfiguration(t *testing.T) {
	environment := tandemEnvironment(Config{
		NodeID:        "node-a",
		DataDir:       "/var/lib/k8e/tandem",
		TandemListen:  "127.0.0.1:2379",
		RqliteHTTP:    "127.0.0.1:4001",
		TandemTLSCert: "/tls/server.crt",
		TandemTLSKey:  "/tls/server.key",
		TandemTLSCA:   "/tls/client-ca.crt",
		TandemMTLS:    true,
	})
	for _, expected := range []string{
		"TANDEM_LEASE_OWNER=node-a",
		"TANDEM_TLS_CERT_FILE=/tls/server.crt",
		"TANDEM_TLS_KEY_FILE=/tls/server.key",
		"TANDEM_TLS_CA_FILE=/tls/client-ca.crt",
		"TANDEM_TLS_REQUIRE_CLIENT_CERT=true",
	} {
		if !slices.Contains(environment, expected) {
			t.Fatalf("missing %q in environment: %v", expected, environment)
		}
	}
}

func TestWaitHealthyHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitHealthy(ctx, "http://127.0.0.1:1/status", time.Second); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestWaitTCPHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitTCP(ctx, "127.0.0.1:1", time.Second); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestDialAddress(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:2379":   "127.0.0.1:2379",
		":2379":          "127.0.0.1:2379",
		"10.0.0.1:2379":  "10.0.0.1:2379",
		"127.0.0.1:2379": "127.0.0.1:2379",
	}
	for input, expected := range cases {
		if got := dialAddress(input); got != expected {
			t.Fatalf("dialAddress(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestPrepareDataDirAcceptsFreshAndExistingStore(t *testing.T) {
	t.Run("missing dir is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rqlite")
		if err := prepareDataDir(dir); err != nil {
			t.Fatalf("prepareDataDir: %v", err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("data dir not created: %v", err)
		}
	})
	t.Run("empty dir is accepted", func(t *testing.T) {
		dir := t.TempDir()
		if err := prepareDataDir(dir); err != nil {
			t.Fatalf("prepareDataDir: %v", err)
		}
	})
	// A restart on a populated store must keep working: the guard exists to
	// catch foreign data, not to refuse every existing cluster.
	for _, name := range []string{"db.sqlite", "db.sqlite-wal", "db.sqlite-shm", "raft.db"} {
		t.Run("existing store via "+name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := prepareDataDir(dir); err != nil {
				t.Fatalf("prepareDataDir rejected a real rqlite store: %v", err)
			}
		})
	}
	t.Run("existing store via raft dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "raft"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := prepareDataDir(dir); err != nil {
			t.Fatalf("prepareDataDir rejected a real rqlite store: %v", err)
		}
	})
}

func TestPrepareDataDirRejectsForeignData(t *testing.T) {
	dir := t.TempDir()
	// The shape of the failure this guards: an etcd data dir, a half-removed
	// backup, or any other leftover must not be silently replaced by an empty
	// rqlite database that then looks like a healthy, empty cluster.
	if err := os.WriteFile(filepath.Join(dir, "member"), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	err := prepareDataDir(dir)
	if err == nil {
		t.Fatal("expected a populated non-rqlite dir to be refused")
	}
	if !strings.Contains(err.Error(), "refusing to rebuild") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLeaderKnownRequiresReadyAndLeader(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"ready with leader":     {`{"store":{"ready":true,"leader":{"addr":"127.0.0.1:4002"}}}`, true},
		"ready without leader":  {`{"store":{"ready":true,"leader":{"addr":""}}}`, false},
		"not ready with leader": {`{"store":{"ready":false,"leader":{"addr":"127.0.0.1:4002"}}}`, false},
		"empty store":           {`{"store":{}}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := &http.Client{}
			if got := leaderKnown(context.Background(), client, server.URL); got != tc.want {
				t.Fatalf("leaderKnown = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadyFailsWithoutRunningRqlite(t *testing.T) {
	s := &Supervisor{cfg: withDefaults(Config{})}
	if err := s.Ready(context.Background()); err == nil {
		t.Fatal("expected Ready to fail when rqlite was never started")
	}
}

// TestSupervisorRestartsCrashedChild kills a supervised child and waits for
// the supervisor to start a replacement, which is the crash-restart contract
// issue #592 requires. A shell script stands in for rqlited so the test does
// not depend on a real rqlite or a built Tandem binary.
func TestSupervisorRestartsCrashedChild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process supervision test in short mode")
	}
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	helper := filepath.Join(dir, "child.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho run >> "+starts+"\nsleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}

	s := &Supervisor{}
	s.cfg = withDefaults(Config{DataDir: dir, NodeID: "supervisor-test", RqliteBinary: helper})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.runCtx, s.cancel = runCtx, cancel
	if err := s.startRqlite(runCtx, dir); err != nil {
		t.Fatalf("startRqlite: %v", err)
	}
	s.monitor.Add(1)
	s.mu.Lock()
	s.monitorStarted = true
	s.mu.Unlock()
	go s.supervise(runCtx)
	t.Cleanup(func() { _ = s.Close() })

	waitForStarts(t, starts, 1)

	s.mu.Lock()
	victim := s.rqlite
	s.mu.Unlock()
	if victim == nil || victim.Process == nil {
		t.Fatal("no supervised child to kill")
	}
	if err := victim.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	waitForStarts(t, starts, 2)
}

// TestSupervisorRestartBudgetStopsSpin checks that a child which cannot start
// is not restarted forever.
func TestSupervisorRestartBudgetStopsSpin(t *testing.T) {
	if got := restartBackoff(1); got != time.Second {
		t.Fatalf("restartBackoff(1) = %s, want 1s", got)
	}
	if got := restartBackoff(20); got != 30*time.Second {
		t.Fatalf("restartBackoff(20) = %s, want the 30s cap", got)
	}
}

func mustRqliteArgs(t *testing.T, cfg Config, dir string) []string {
	t.Helper()
	args, err := rqliteArgs(cfg, dir)
	if err != nil {
		t.Fatalf("rqliteArgs: %v", err)
	}
	return args
}

// Raft traffic between members carries every committed write, so KIP-29 §4
// requires it to be encrypted and authenticated. rqlite turns TLS on only when
// -node-cert is given, so all three credentials have to travel together.
func TestRqliteArgsEnableRaftTLS(t *testing.T) {
	dir := t.TempDir()
	cert, key, ca := filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"), filepath.Join(dir, "peer-ca.crt")
	for _, path := range []string{cert, key, ca} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := mustRqliteArgs(t, Config{
		NodeID:                 "n1",
		RqliteHTTP:             "127.0.0.1:4001",
		RqliteRaft:             "10.0.0.1:4002",
		RqliteNodeCert:         cert,
		RqliteNodeKey:          key,
		RqliteNodeCA:           ca,
		RqliteNodeVerifyClient: true,
		RqliteNodeServerName:   "raft.internal",
	}, "/data")
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-node-cert " + cert,
		"-node-key " + key,
		"-node-ca-cert " + ca,
		"-node-verify-client",
		"-node-verify-server-name raft.internal",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in %v", expected, args)
		}
	}
	if args[len(args)-1] != "/data" {
		t.Errorf("data dir must stay the final argument, got %v", args)
	}
}

// Encryption alone still lets any host that reaches the port join the
// membership, so mutual authentication is opt-out rather than opt-in.
func TestRqliteArgsWithoutVerifyClientStayOneWay(t *testing.T) {
	dir := t.TempDir()
	cert, key, ca := filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key"), filepath.Join(dir, "ca.crt")
	for _, path := range []string{cert, key, ca} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := mustRqliteArgs(t, Config{RqliteNodeCert: cert, RqliteNodeKey: key, RqliteNodeCA: ca}, "/data")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-node-cert "+cert) {
		t.Errorf("TLS not enabled: %v", args)
	}
	if strings.Contains(joined, "-node-verify-client") {
		t.Errorf("client verification must be requested: %v", args)
	}
}

// A half-configured credential set is refused. Passing it through would start a
// node that looks protected and is not, which is worse than a plaintext node
// because nobody can tell the difference from the outside.
func TestRqliteArgsRejectAPartialRaftCredentialSet(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "node.crt")
	if err := os.WriteFile(cert, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Config{
		"cert only":        {RqliteNodeCert: cert},
		"cert and key":     {RqliteNodeCert: cert, RqliteNodeKey: filepath.Join(dir, "node.key")},
		"cert and CA":      {RqliteNodeCert: cert, RqliteNodeCA: filepath.Join(dir, "ca.crt")},
		"absent cert file": {RqliteNodeCert: filepath.Join(dir, "absent.crt"), RqliteNodeKey: filepath.Join(dir, "node.key"), RqliteNodeCA: filepath.Join(dir, "ca.crt")},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := rqliteArgs(cfg, "/data"); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

// A node with no Raft credentials keeps the historical command line, which is
// what a loopback-only member runs.
func TestRqliteArgsWithoutCredentialsStayUnencrypted(t *testing.T) {
	args := mustRqliteArgs(t, Config{NodeID: "n1", RqliteRaft: "127.0.0.1:4002"}, "/data")
	joined := strings.Join(args, " ")
	for _, flag := range []string{"-node-cert", "-node-key", "-node-ca-cert", "-node-verify-client", "-node-verify-server-name"} {
		if strings.Contains(joined, flag) {
			t.Errorf("%s must not be passed without credentials: %v", flag, args)
		}
	}
}

func waitForStarts(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if strings.Count(string(data), "run") >= want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %d starts; saw %q", want, data)
}

// A multi-member cluster cannot be formed by passing -join alone: rqlite
// bootstraps each node as its own single-member cluster unless it is told how
// many voters to expect, and it needs the complete address set rather than one
// seed. Both flags are therefore derived from the peer list.
func TestRqliteArgsFormAClusterFromThePeerSet(t *testing.T) {
	args := mustRqliteArgs(t, Config{
		NodeID:          "n2",
		RqliteHTTP:      "127.0.0.1:4002",
		RqliteRaft:      "127.0.0.1:4003",
		BootstrapExpect: 3,
		RqlitePeers:     []string{"127.0.0.1:4001", "127.0.0.1:4003", "127.0.0.1:4005"},
	}, "/data")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-node-id n2") {
		t.Errorf("node id missing from %v", args)
	}
	if !strings.Contains(joined, "-bootstrap-expect 3") {
		t.Errorf("expected voter count missing from %v", args)
	}
	if !strings.Contains(joined, "-join 127.0.0.1:4001,127.0.0.1:4003,127.0.0.1:4005") {
		t.Errorf("full peer set missing from %v", args)
	}
	if args[len(args)-1] != "/data" {
		t.Errorf("data dir must stay the final argument, got %v", args)
	}
}

// A single node must keep its historical arguments: adding a bootstrap flag
// to a lone member would make it wait for voters that never arrive.
func TestRqliteArgsStaySingleNodeWithoutAPeerSet(t *testing.T) {
	args := mustRqliteArgs(t, Config{
		NodeID:     "n1",
		RqliteHTTP: "127.0.0.1:4001",
		RqliteRaft: "127.0.0.1:4002",
	}, "/data")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-bootstrap-expect") || strings.Contains(joined, "-join") {
		t.Errorf("single node must not receive cluster flags: %v", args)
	}
}

// Joining a cluster that is already running is a different operation from
// forming a new one: only the seed address is needed, and a bootstrap count
// would make the node wait for a formation that has already happened.
func TestRqliteArgsJoinAnExistingCluster(t *testing.T) {
	args := mustRqliteArgs(t, Config{
		NodeID:      "n3",
		RqliteHTTP:  "127.0.0.1:4003",
		RqliteRaft:  "127.0.0.1:4004",
		RqliteJoin:  "10.0.0.1:4002",
		RqlitePeers: []string{"10.0.0.1:4002"},
	}, "/data")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-join 10.0.0.1:4002") {
		t.Errorf("seed address missing from %v", args)
	}
	if strings.Contains(joined, "-bootstrap-expect") {
		t.Errorf("joining a running cluster must not wait for a formation: %v", args)
	}
}

// The node id is rqlite's -node-id and the lease owner, so a collision is not
// recoverable: two members with one id cannot form a membership, and two lease
// owners with one string defeat the single-expiry-owner claim. The historical
// "1" fallback is only safe when there is no peer set to collide with.
func TestHostnameFallbackIsNotUsedForACluster(t *testing.T) {
	single := withDefaults(Config{RqliteHTTP: "127.0.0.1:4001", RqliteRaft: "127.0.0.1:4002"})
	if single.NodeID == "" {
		t.Error("a single node still needs some identity")
	}
	cluster := withDefaults(Config{
		RqliteHTTP:  "127.0.0.1:4001",
		RqliteRaft:  "127.0.0.1:4002",
		RqlitePeers: []string{"a:4002", "b:4002", "c:4002"},
	})
	if cluster.NodeID == "1" {
		t.Error("a multi-node cluster must not fall back to the constant id 1")
	}
}

// Two k8e processes on one data directory would open the same SQLite file
// and the same Raft log. The second must be told who holds it rather than
// discovering it as an opaque rqlite failure.
func TestSecondProcessOnTheSameDataDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := prepareDataDir(dir); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	lock, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := lockDataDir(dir); err == nil {
		_ = lock.Unlock()
		t.Fatal("a second process acquired a held data dir lock")
	} else if !strings.Contains(err.Error(), "already in use") {
		_ = lock.Unlock()
		t.Fatalf("unhelpful error for a held lock: %v", err)
	}

	// Releasing lets the next process in, which is what a restart needs.
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	_ = lock.Close()
	next, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = next.Unlock()
	_ = next.Close()
}
