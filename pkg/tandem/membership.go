package tandem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrNoQuorum reports that the rqlite voting group has shrunk past the point
// where it can commit. It is wrapped rather than returned bare so a caller can
// distinguish "the cluster cannot serve" from "I could not reach it", which
// lead to opposite remedies: the first needs members brought back, the second
// is a network or process problem.
var ErrNoQuorum = errors.New("rqlite has no quorum")

// Node is one member of the rqlite voting group as /nodes reports it.
type Node struct {
	// ID is rqlite's -node-id for the member, which is also the lease owner
	// in the compat layer.
	ID string
	// Addr is the member's Raft address.
	Addr string
	// Voter distinguishes a full member from a read replica. Only voters
	// count towards quorum.
	Voter bool
	// Reachable reports whether the group can currently talk to the member.
	Reachable bool
	Leader    bool
}

// Membership is one node's view of the voting group.
//
// It is the only place k8e reads the group's shape: whether this node may
// serve, and whether a member is in it at all.
type Membership struct {
	Nodes []Node
}

// Voters returns the voting members, reachable or not. Read replicas are
// excluded because Raft does not count them, and including them would
// overstate both the quorum needed and the quorum held.
func (m Membership) Voters() []Node {
	var voters []Node
	for _, node := range m.Nodes {
		if node.Voter {
			voters = append(voters, node)
		}
	}
	return voters
}

// Reachable counts the voters the group can currently talk to.
func (m Membership) Reachable() int {
	count := 0
	for _, node := range m.Voters() {
		if node.Reachable {
			count++
		}
	}
	return count
}

// Quorum is the number of voters that must be reachable before a write can
// commit: a strict majority.
func (m Membership) Quorum() int { return quorumFor(len(m.Voters())) }

// HasQuorum reports whether the group can still commit.
func (m Membership) HasQuorum() bool { return m.Reachable() >= m.Quorum() }

// QuorumError describes a group that cannot commit, naming the counts an
// operator needs: how many voters exist, how many are reachable, how many are
// needed.
func (m Membership) QuorumError() error {
	voters := m.Voters()
	return fmt.Errorf("%w: %d of %d voters reachable, %d needed", ErrNoQuorum, m.Reachable(), len(voters), m.Quorum())
}

// HasVoter reports whether id is a voting member, whether or not the group can
// reach it. That is the question a removal has to answer: a member that is
// listed but unreachable still has to be removed, and a member that is not
// listed at all is already out.
func (m Membership) HasVoter(id string) bool {
	for _, node := range m.Voters() {
		if node.ID == id {
			return true
		}
	}
	return false
}

// quorumFor is the Raft majority, (N/2)+1. A one-member group needs itself;
// three members need two; four still need three, which is why even-sized
// groups buy no extra fault tolerance.
func quorumFor(voters int) int { return voters/2 + 1 }

// membershipClient reads and changes the group through the HTTP API of one
// rqlite node. Membership requests may be sent to any member: rqlite forwards
// what only the leader can service.
type membershipClient struct {
	base   string
	client *http.Client
}

// membershipTimeout bounds a single membership request. These are local calls
// against this node's own rqlite, so a long timeout would only delay an answer
// that is not going to change.
const membershipTimeout = 5 * time.Second

// nodesPath asks for the non-voting members too. The response marks each entry,
// so asking for everything and filtering here is unambiguous; asking for only
// voters and assuming they are all voters would silently count a read replica.
const nodesPath = "/nodes?nonvoters"

const removePath = "/remove"

func newMembershipClient(base string) *membershipClient {
	return &membershipClient{
		base:   strings.TrimSuffix(base, "/"),
		client: &http.Client{Timeout: membershipTimeout},
	}
}

// membershipEntry is the shape of one value in rqlite's /nodes response. The
// map is keyed by member identity, which is also carried in "id".
type membershipEntry struct {
	ID        string `json:"id"`
	Addr      string `json:"addr"`
	Voter     bool   `json:"voter"`
	Reachable bool   `json:"reachable"`
	Leader    bool   `json:"leader"`
}

func (c *membershipClient) nodes(ctx context.Context) (Membership, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+nodesPath, http.NoBody)
	if err != nil {
		return Membership{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return Membership{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Membership{}, fmt.Errorf("read rqlite membership: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Membership{}, fmt.Errorf("rqlite membership: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var entries map[string]membershipEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return Membership{}, fmt.Errorf("decode rqlite membership: %w", err)
	}
	membership := Membership{}
	for key, entry := range entries {
		id := entry.ID
		if id == "" {
			id = key
		}
		membership.Nodes = append(membership.Nodes, Node{
			ID:        id,
			Addr:      entry.Addr,
			Voter:     entry.Voter,
			Reachable: entry.Reachable,
			Leader:    entry.Leader,
		})
	}
	// Map iteration is unordered; sorting keeps the view stable for logs and
	// for a test that compares two readings.
	sort.Slice(membership.Nodes, func(i, j int) bool { return membership.Nodes[i].ID < membership.Nodes[j].ID })
	return membership, nil
}

// removeRequest is rqlite's /remove body.
//
// It carries the node id and nothing else. Current rqlite parses the body
// strictly and answers 400 to an unrecognised field, so including the Raft
// address — which older releases matched on — is refused outright rather than
// ignored, and the removal then never happens.
type removeRequest struct {
	ID string `json:"id"`
}

// remove drops a member from the group.
//
// This is a Raft operation, so it needs a leader: against a group that has
// lost quorum it fails, and that failure has to reach the caller rather than
// being reported as a removal that took effect. Any member may be asked —
// rqlite forwards the request to the leader — so this node's own API is
// enough even when it is not the leader.
func (c *membershipClient) remove(ctx context.Context, id string) error {
	body, err := json.Marshal(removeRequest{ID: id})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+removePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read rqlite removal response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rqlite remove %s: %s: %s", id, resp.Status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// waitUntilVoter waits for id to appear as a voting member.
//
// A node that was told to join has joined only once the group lists it.
// /readyz cannot answer this on its own: a node that failed to reach its seed
// and formed a group of its own is ready too, and the difference is invisible
// at the socket. Membership changes are committed through Raft, so the listing
// is the earliest honest evidence that the join was applied. Errors are retried
// until the deadline because /nodes refuses while the group is still forming.
func waitUntilVoter(ctx context.Context, client *membershipClient, id string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var last error
	for {
		membership, err := client.nodes(ctx)
		if err == nil && membership.HasVoter(id) {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if last != nil {
				return fmt.Errorf("%s is not a voting member: %w", id, last)
			}
			return fmt.Errorf("%s is not a voting member; rqlite reports %d of them", id, len(membership.Voters()))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitQuorum waits until a majority of the voters is reachable.
//
// A known leader is not the same as a quorum. The two are checked separately
// because they fail differently: a group that has lost quorum has no leader to
// name, and a group that is still forming reports neither, so polling both
// keeps a transient election from being reported as an outage.
func waitQuorum(ctx context.Context, client *membershipClient, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var membership Membership
	var last error
	for {
		membership, last = client.nodes(ctx)
		if last == nil && membership.HasQuorum() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if last != nil {
				return fmt.Errorf("could not read the rqlite membership: %w", last)
			}
			return membership.QuorumError()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
