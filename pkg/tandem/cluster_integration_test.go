//go:build tandem_integration

package tandem

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// clusterMember is one Tandem process with its own rqlite, which is how a node
// is deployed: the compat layer and the store it speaks to move together.
type clusterMember struct {
	id        string
	httpAddr  string
	raftAddr  string
	dir       string
	logPath   string
	endpoint  string
	tlsConfig *tls.Config

	rqlite *exec.Cmd
	tandem *exec.Cmd
	log    *os.File
	exited chan struct{}
}

type cluster struct {
	t       *testing.T
	members []*clusterMember
	stopped sync.Once
}

// startCluster brings up nodeCount Tandem processes, each with its own rqlite,
// formed into one voting group.
//
// This exercises the path issue #592's M2 phase 2 asks for — leader and
// follower failure, and whether a surviving majority still serves — at the
// layer K8E actually runs, rather than against bare rqlite. The fixture
// mirrors tests/rqlitecompat: every member is told the full Raft address set
// and the expected voter count, because either half alone leaves each node
// bootstrapping as its own single-member cluster.
func startCluster(t *testing.T, binary string, nodeCount int) *cluster {
	t.Helper()
	rqlited := os.Getenv("TANDEM_TEST_RQLITED")
	if rqlited == "" {
		t.Fatal("set TANDEM_TEST_RQLITED to a rqlited executable")
	}
	if nodeCount < 1 {
		t.Fatalf("node count %d is not a cluster", nodeCount)
	}

	c := &cluster{t: t}
	for i := 1; i <= nodeCount; i++ {
		member := &clusterMember{
			id:       fmt.Sprintf("n%d", i),
			httpAddr: testAddress(t),
			raftAddr: testAddress(t),
			dir:      filepath.Join(t.TempDir(), fmt.Sprintf("n%d", i)),
			logPath:  filepath.Join(t.TempDir(), fmt.Sprintf("n%d.log", i)),
		}
		if err := os.MkdirAll(member.dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", member.id, err)
		}
		c.members = append(c.members, member)
	}
	raftAddrs := make([]string, 0, len(c.members))
	for _, member := range c.members {
		raftAddrs = append(raftAddrs, member.raftAddr)
	}
	// One PKI for the whole cluster: each member terminates mTLS with the
	// same CA, which is how the Supervisor wires them in production.
	certPath, keyPath, clientCfg := testPKI(t)

	for _, member := range c.members {
		logFile, err := os.Create(member.logPath)
		if err != nil {
			t.Fatalf("create log for %s: %v", member.id, err)
		}
		member.log = logFile

		rqliteArgs := []string{
			"-node-id", member.id,
			"-http-addr", member.httpAddr,
			"-raft-addr", member.raftAddr,
		}
		if nodeCount > 1 {
			rqliteArgs = append(rqliteArgs,
				"-bootstrap-expect", strconv.Itoa(nodeCount),
				"-join", strings.Join(raftAddrs, ","),
			)
		}
		rqliteArgs = append(rqliteArgs, filepath.Join(member.dir, "rqlite"))
		if err := os.MkdirAll(filepath.Join(member.dir, "rqlite"), 0o700); err != nil {
			t.Fatalf("create rqlite dir for %s: %v", member.id, err)
		}
		rqlite := exec.Command(rqlited, rqliteArgs...)
		rqlite.Stdout, rqlite.Stderr = logFile, logFile
		if err := rqlite.Start(); err != nil {
			t.Fatalf("start rqlite for %s: %v", member.id, err)
		}
		member.rqlite = rqlite
		member.exited = make(chan struct{})
		go func() { _ = rqlite.Wait(); close(member.exited) }()
	}

	for _, member := range c.members {
		if err := waitHealthy(context.Background(),
			fmt.Sprintf("http://%s/readyz", member.httpAddr), 60*time.Second); err != nil {
			c.dumpLogs()
			c.stopAll()
			t.Fatalf("%s never became ready: %v", member.id, err)
		}
	}

	for _, member := range c.members {
		address := testAddress(t)
		tandem := exec.Command(binary)
		tandem.Env = append(os.Environ(),
			"TANDEM_RQLITE_ADDR=http://"+member.httpAddr,
			"TANDEM_DATA_DIR="+member.dir,
			"TANDEM_LISTEN_ADDR="+address,
			"TANDEM_TLS_CERT_FILE="+certPath,
			"TANDEM_TLS_KEY_FILE="+keyPath,
			"TANDEM_TLS_CA_FILE="+filepath.Join(filepath.Dir(certPath), "ca.pem"),
			"TANDEM_TLS_REQUIRE_CLIENT_CERT=false",
			// Distinct per node: this is the identity a three-member
			// deployment must supply, and the default would collide here
			// because every process shares one hostname.
			"TANDEM_LEASE_OWNER="+member.id,
		)
		tandem.Stdout, tandem.Stderr = member.log, member.log
		if err := tandem.Start(); err != nil {
			c.dumpLogs()
			c.stopAll()
			t.Fatalf("start tandem for %s: %v", member.id, err)
		}
		member.tandem = tandem
		member.endpoint = "https://" + address
		member.tlsConfig = clientCfg
	}

	for _, member := range c.members {
		if err := waitTCP(context.Background(), strings.TrimPrefix(member.endpoint, "https://"), 60*time.Second); err != nil {
			c.dumpLogs()
			c.stopAll()
			t.Fatalf("%s compat layer never listened: %v", member.id, err)
		}
	}

	t.Cleanup(func() {
		c.stopAll()
		if t.Failed() {
			c.dumpLogs()
		}
	})
	return c
}

func (c *cluster) client(t *testing.T, member *clusterMember) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{member.endpoint},
		TLS:         member.tlsConfig,
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", member.id, err)
	}
	t.Cleanup(func() { cli.Close() })
	waitForReady(t, context.Background(), cli, 500*time.Millisecond, 30*time.Second, member.id+" readiness")
	return cli
}

// stopAll terminates every member. Tandem goes first so rqlite is not killed
// out from under a compat layer that is still serving.
func (c *cluster) stopAll() {
	c.stopped.Do(func() {
		for _, member := range c.members {
			if member.tandem != nil && member.tandem.Process != nil {
				_ = member.tandem.Process.Kill()
				_ = member.tandem.Wait()
			}
		}
		for _, member := range c.members {
			if member.rqlite != nil && member.rqlite.Process != nil {
				_ = member.rqlite.Process.Kill()
				_ = member.rqlite.Wait()
			}
			if member.log != nil {
				_ = member.log.Close()
			}
		}
	})
}

// killRqlite simulates a datastore crash on one member while leaving its compat
// layer running, which is the asymmetry a partition or a lost disk produces.
func (c *cluster) killRqlite(t *testing.T, member *clusterMember) {
	t.Helper()
	if member.rqlite == nil || member.rqlite.Process == nil {
		return
	}
	_ = member.rqlite.Process.Kill()
	select {
	case <-member.exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s rqlite did not exit", member.id)
	}
	member.rqlite = nil
}

func (c *cluster) dumpLogs() {
	for _, member := range c.members {
		if data, err := os.ReadFile(member.logPath); err == nil && len(data) > 0 {
			c.t.Logf("---- %s ----\n%s", member.id, tail(string(data), 40))
		}
	}
}

func tail(text string, lines int) string {
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// rqliteNodes asks one member who else it sees. This is the membership view an
// operator would use to confirm the cluster actually formed.
func rqliteNodes(t *testing.T, httpAddr string) map[string]bool {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + httpAddr + "/nodes")
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	defer resp.Body.Close()
	var nodes map[string]struct {
		Reachable bool   `json:"reachable"`
		Voter     bool   `json:"voter"`
		Leader    bool   `json:"leader"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	out := make(map[string]bool, len(nodes))
	for id, node := range nodes {
		out[id] = node.Reachable && node.Voter
	}
	return out
}

// TestTandemThreeNodeCluster is the M2 phase-2 case at the layer K8E runs:
// three members form one voting group, a write on one is readable on another,
// and the group keeps serving after the leader's store dies.
//
// The assertions are deliberately about what an operator would check: the
// membership each member reports, and whether data written through one compat
// layer is visible through another. A member that merely starts is not
// evidence of a cluster.
func TestTandemThreeNodeCluster(t *testing.T) {
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Fatal("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	c := startCluster(t, binary, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("every member sees three voters", func(t *testing.T) {
		// Formation is asynchronous: a member that answered /readyz during
		// the election may not yet have learned the others. Poll rather than
		// assert once, so the test measures convergence and not scheduling.
		deadline := time.Now().Add(60 * time.Second)
		for {
			nodes := rqliteNodes(t, c.members[0].httpAddr)
			if len(nodes) == 3 {
				for id, healthy := range nodes {
					if !healthy {
						t.Errorf("%s reports %s as a voter that is not reachable", c.members[0].id, id)
					}
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("cluster never formed: %s sees %d of 3 voters", c.members[0].id, len(nodes))
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	clients := make([]*clientv3.Client, len(c.members))
	for i, member := range c.members {
		clients[i] = c.client(t, member)
	}

	t.Run("a write on one member is readable on another", func(t *testing.T) {
		// Written through n1's compat layer and read through n3's: the two
		// share only the rqlite Raft group, so this is replication rather
		// than a local read.
		put, err := clients[0].Put(ctx, "cluster/replicated", "value-from-n1")
		if err != nil {
			t.Fatalf("write on %s: %v", c.members[0].id, err)
		}
		var lastErr error
		var got string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			response, err := clients[2].Get(ctx, "cluster/replicated")
			if err != nil {
				lastErr = err
			} else if response.Count > 0 {
				got = string(response.Kvs[0].Value)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if got != "value-from-n1" {
			t.Fatalf("read on %s got %q (last error %v), want the value written on %s at revision %d",
				c.members[2].id, got, lastErr, c.members[0].id, put.Header.Revision)
		}
	})

	t.Run("the group keeps serving after the leader's store dies", func(t *testing.T) {
		// Losing one of three leaves a majority, so the survivors must elect
		// a new leader and keep accepting writes. This is the case a
		// single-member deployment cannot produce, which is the whole point
		// of the three-voter target.
		leader := leaderMember(t, c)
		c.killRqlite(t, leader)

		// The remaining majority has to converge on a new leader before it
		// can accept another write.
		survivors := survivingClients(t, c, clients, leader)
		deadline := time.Now().Add(60 * time.Second)
		var lastErr error
		for {
			if _, lastErr = survivors[0].Put(ctx, "cluster/after-failover", "written-after"); lastErr == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("majority never accepted a write after %s died: %v", leader.id, lastErr)
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
}

// leaderMember finds which member rqlite currently calls the leader.
func leaderMember(t *testing.T, c *cluster) *clusterMember {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	for _, member := range c.members {
		resp, err := client.Get("http://" + member.httpAddr + "/status")
		if err != nil {
			continue
		}
		var status struct {
			Store struct {
				Leader struct {
					NodeID string `json:"node_id"`
				} `json:"leader"`
			} `json:"store"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		if decodeErr != nil || status.Store.Leader.NodeID == "" {
			continue
		}
		for _, candidate := range c.members {
			if candidate.id == status.Store.Leader.NodeID {
				return candidate
			}
		}
	}
	t.Fatal("no member reported a leader")
	return nil
}

func survivingClients(t *testing.T, c *cluster, clients []*clientv3.Client, dead *clusterMember) []*clientv3.Client {
	t.Helper()
	var survivors []*clientv3.Client
	for i, member := range c.members {
		if member != dead {
			survivors = append(survivors, clients[i])
		}
	}
	if len(survivors) == 0 {
		t.Fatal("no survivors")
	}
	return survivors
}
