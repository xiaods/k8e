package tandem

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The quorum table is the whole reason a group size is chosen: an even-sized
// group needs one more voter than the odd-sized group below it and tolerates
// exactly the same number of failures, so there is nothing to gain from it.
func TestQuorumFor(t *testing.T) {
	cases := map[int]int{1: 1, 2: 2, 3: 2, 4: 3, 5: 3, 6: 4, 7: 4, 9: 5}
	for voters, want := range cases {
		if got := quorumFor(voters); got != want {
			t.Errorf("quorumFor(%d) = %d, want %d", voters, got, want)
		}
	}
}

// nodesBody is the shape rqlite answers /nodes with: a map keyed by member
// identity, each entry carrying the id, the Raft address and its role.
func nodesBody(t *testing.T, nodes ...Node) string {
	t.Helper()
	entries := make(map[string]membershipEntry, len(nodes))
	for _, node := range nodes {
		// Converted rather than copied field by field: the two types are the
		// same shape, and a conversion cannot fall behind when one of them
		// gains a field.
		entries[node.ID] = membershipEntry(node)
	}
	body, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// voter builds a member the way /nodes reports it: a voter at an address of
// its own, reachable or not depending on what the case needs.
func voter(id string, reachable bool) Node {
	return Node{ID: id, Addr: id + ".raft:4002", Voter: true, Reachable: reachable}
}

// A read replica receives the whole log but does not vote, so counting it
// would overstate both the quorum the group holds and the quorum it needs —
// the two errors go in opposite directions and do not cancel out.
func TestMembershipCountsOnlyVoters(t *testing.T) {
	client := newMembershipClient("http://127.0.0.1:1")
	body := nodesBody(t, voter("n1", true), voter("n2", true), voter("n3", false), Node{ID: "replica", Voter: false, Reachable: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client.base = server.URL

	membership, err := client.nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if len(membership.Voters()) != 3 {
		t.Fatalf("voters = %d, want 3: %+v", len(membership.Voters()), membership.Nodes)
	}
	if membership.Quorum() != 2 {
		t.Errorf("quorum = %d, want 2", membership.Quorum())
	}
	if membership.Reachable() != 2 {
		t.Errorf("reachable = %d, want 2", membership.Reachable())
	}
	if !membership.HasQuorum() {
		t.Error("two of three voters is a quorum")
	}
	if !membership.HasVoter("n3") {
		t.Error("a voter that is unreachable is still a member, and still has to be removed")
	}
	if membership.HasVoter("replica") {
		t.Error("a read replica must not be counted as a voter")
	}
}

// The case the three-member work is actually about: a minority must know it
// cannot serve, and must be able to say why.
func TestMembershipWithoutAMajorityHasNoQuorum(t *testing.T) {
	client := newMembershipClient("http://127.0.0.1:1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nodesBody(t, voter("n1", true), voter("n2", false), voter("n3", false))))
	}))
	defer server.Close()
	client.base = server.URL

	membership, err := client.nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if membership.HasQuorum() {
		t.Fatal("one of three voters must not be a quorum")
	}
	err = membership.QuorumError()
	if !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("QuorumError = %v, want ErrNoQuorum", err)
	}
	for _, want := range []string{"1 of 3", "2 needed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// The window a caller has to wait out: every voter is listed and reachable,
// and the group still cannot commit, because it has not agreed on a leader.
// Readiness and a quorum both hold here, which is why neither is enough on its
// own before a write is attempted.
func TestMembershipWithQuorumButNoLeaderCannotServe(t *testing.T) {
	client := newMembershipClient("http://127.0.0.1:1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nodesBody(t, voter("n1", true), voter("n2", true), voter("n3", true))))
	}))
	defer server.Close()
	client.base = server.URL

	membership, err := client.nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if !membership.HasQuorum() {
		t.Fatal("three of three voters is a quorum, leader or not")
	}
	if membership.HasLeader() {
		t.Fatal("a group that names no leader must not report one")
	}
	if _, ok := membership.Leader(); ok {
		t.Error("Leader() must not return a node when the group has none")
	}
}

// The same group once it has settled. Naming the leader is what turns
// "the members are present" into "the group can serve".
func TestMembershipNamesItsLeader(t *testing.T) {
	client := newMembershipClient("http://127.0.0.1:1")
	leader := voter("n2", true)
	leader.Leader = true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nodesBody(t, voter("n1", true), leader, voter("n3", true))))
	}))
	defer server.Close()
	client.base = server.URL

	membership, err := client.nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	node, ok := membership.Leader()
	if !ok {
		t.Fatal("the group names a leader")
	}
	if node.ID != "n2" {
		t.Errorf("leader = %q, want n2", node.ID)
	}
	if !membership.HasLeader() {
		t.Error("HasLeader must agree with Leader")
	}
}

// A node that is not the leader refuses the request rather than answering with
// an old membership, and that refusal is an error rather than an empty group.
func TestMembershipSurfacesAServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "node is not the leader", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newMembershipClient(server.URL)
	if _, err := client.nodes(context.Background()); err == nil {
		t.Fatal("expected a non-200 membership response to be an error")
	}
}

// A removal is identified by the member's node id, and by nothing else:
// current rqlite parses the body strictly and answers 400 to a body carrying
// an unrecognised field, so a request that looks harmlessly more complete is
// refused and the member is never removed.
func TestRemoveRequestsTheNodeIdentity(t *testing.T) {
	type record struct {
		method string
		path   string
		body   string
	}
	var got record
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got = record{r.Method, r.URL.Path, string(buf)}
	}))
	defer server.Close()

	if err := newMembershipClient(server.URL).remove(context.Background(), "n2"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got.method != http.MethodDelete || got.path != "/remove" {
		t.Fatalf("remove sent %s %s", got.method, got.path)
	}
	var request removeRequest
	if err := json.Unmarshal([]byte(got.body), &request); err != nil {
		t.Fatalf("body %q is not the removal request: %v", got.body, err)
	}
	if request.ID != "n2" {
		t.Errorf("removal request = %+v", request)
	}
	if got.body != `{"id":"n2"}` {
		t.Errorf("the body must carry the node id alone, got %s", got.body)
	}
}

// rqlite refuses a removal it cannot service, and that refusal has to reach
// the caller rather than being reported as a removal that took effect.
func TestRemoveSurfacesTheServersAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no leader", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	err := newMembershipClient(server.URL).remove(context.Background(), "n2")
	if err == nil {
		t.Fatal("expected a refused removal to be an error")
	}
	if !strings.Contains(err.Error(), "n2") {
		t.Errorf("error %q does not name the member being removed", err)
	}
}

// A join is only real once the group lists the node. The first reading is what
// a member sees while the join is still being committed; the second is after.
func TestWaitUntilVoterWaitsForTheJoin(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		body := nodesBody(t, voter("n1", true), voter("n2", true))
		if calls > 1 {
			body = nodesBody(t, voter("n1", true), voter("n2", true), voter("n3", true))
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	if err := waitUntilVoter(context.Background(), newMembershipClient(server.URL), "n3", 10*time.Second); err != nil {
		t.Fatalf("waitUntilVoter: %v", err)
	}
	if calls < 2 {
		t.Error("a join that is not listed yet must be waited for, not accepted")
	}
}

// The failure mode the wait exists to catch: a node that was told to join and
// never did has to be reported as a failure to join, not waited for forever.
func TestWaitUntilVoterGivesUpOnANodeThatNeverJoins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nodesBody(t, voter("n1", true), voter("n2", true))))
	}))
	defer server.Close()
	err := waitUntilVoter(context.Background(), newMembershipClient(server.URL), "n3", 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected a node that never appears in the membership to be reported")
	}
	if !strings.Contains(err.Error(), "n3") {
		t.Errorf("error %q does not name the member", err)
	}
}

// Two of three is a quorum even though one member is down, which is the case a
// cluster exists for.
func TestWaitQuorumAcceptsAMajority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nodesBody(t, voter("n1", true), voter("n2", true), voter("n3", false))))
	}))
	defer server.Close()
	if err := waitQuorum(context.Background(), newMembershipClient(server.URL), 5*time.Second); err != nil {
		t.Fatalf("waitQuorum: %v", err)
	}
}

// The case that has to be caught before anything is served: one voter out of
// three, reported as ErrNoQuorum rather than as a node that could not be read.
func TestWaitQuorumRejectsAMinority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(nodesBody(t, voter("n1", true), voter("n2", false), voter("n3", false))))
	}))
	defer server.Close()
	err := waitQuorum(context.Background(), newMembershipClient(server.URL), 500*time.Millisecond)
	if !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("waitQuorum = %v, want ErrNoQuorum", err)
	}
}

// The readiness contract a member has to hold after quorum is lost: the
// process is up, its port accepts connections, and it still must not be
// reported as able to serve.
func TestReadyRejectsAMemberThatLostQuorum(t *testing.T) {
	ready := true
	minority := nodesBody(t, voter("n1", true), voter("n2", false), voter("n3", false))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rqliteReadyzPath:
			if !ready {
				http.Error(w, "no leader", http.StatusServiceUnavailable)
				return
			}
		case "/status":
			_, _ = w.Write([]byte(`{"store":{"ready":true,"leader":{"addr":"127.0.0.1:4002"}}}`))
			return
		case "/nodes":
			_, _ = w.Write([]byte(minority))
			return
		}
	}))
	defer server.Close()

	s := &Supervisor{cfg: withDefaults(Config{RqliteHTTP: strings.TrimPrefix(server.URL, "http://")})}
	err := s.ready(context.Background())
	if !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("a member that lost quorum reported ready: %v", err)
	}

	// With quorum restored the same node is ready again, so the check is
	// measuring the group and not merely refusing.
	minority = nodesBody(t, voter("n1", true), voter("n2", true), voter("n3", false))
	if err := s.ready(context.Background()); err != nil {
		t.Fatalf("a member with a majority must be ready: %v", err)
	}

	// A node with no leader at all fails earlier, and for a different reason.
	ready = false
	if err := s.ready(context.Background()); errors.Is(err, ErrNoQuorum) {
		t.Fatal("a node with no leader must not be reported as a quorum problem")
	}
}

// A member that cannot read the membership at all is not a member that has
// quorum; reporting those as the same thing would send an operator to fix the
// wrong problem.
func TestReadyDistinguishesAnUnreadableMembership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rqliteReadyzPath:
		case "/status":
			_, _ = w.Write([]byte(`{"store":{"ready":true,"leader":{"addr":"127.0.0.1:4002"}}}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	s := &Supervisor{cfg: withDefaults(Config{RqliteHTTP: strings.TrimPrefix(server.URL, "http://")})}
	if err := s.ready(context.Background()); err == nil || errors.Is(err, ErrNoQuorum) {
		t.Fatalf("an unreadable membership must not be reported as a quorum verdict: %v", err)
	}
}

// Which configurations make this node part of a group: being told one peer's
// address is not the same as being told to join it, and only the latter is a
// cluster this node has to be a member of.
func TestClustered(t *testing.T) {
	cases := map[string]struct {
		cfg  Config
		want bool
	}{
		"lone member":       {Config{}, false},
		"one peer":          {Config{RqlitePeers: []string{"10.0.0.1:4002"}}, false},
		"joining a cluster": {Config{RqliteJoin: "10.0.0.1:4002"}, true},
		"forming a cluster": {Config{BootstrapExpect: 3, RqlitePeers: []string{"a:1", "b:1", "c:1"}}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := clustered(tc.cfg); got != tc.want {
				t.Errorf("clustered(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
