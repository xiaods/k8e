// Package rqlitecompat is the executable M0 prototype for Issue #614: the
// minimal proof that rqlite's public HTTP API can express the etcd v3 storage
// semantics the compatibility layer (M1) must provide.
//
// The prototype is deliberately small. It implements only what M0 has to
// prove — atomic transactions with revision allocation, a response taken from
// the same transaction that mutates, exactly-once replay of an uncertain
// request, and explicit linearizable reads — and it is driven against a real
// rqlited process by tests in this package.
//
// The etcd call surface the prototype mirrors was read from the tree and the
// dependency sources, never from memory:
//
//   - k8s.io/apiserver/pkg/storage/etcd3 (store/watcher/compact/lease_manager)
//   - go.etcd.io/etcd/client/v3/kubernetes (k3s fork used by the apiserver)
//   - pkg/etcdstorage, pkg/etcd (K8E's own etcd users)
//
// See docs/kip-29-rqlite-etcd-compat-backend.md: the design, the pinned
// rqlite version and Appendix A (the etcd API/semantics matrix).
package rqlitecompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Timeouts passed to rqlite on every request. db_timeout bounds a single SQL
// statement; timeout bounds a follower's forwarding wait for the leader
// (rqlite default 30s) and linearizable_timeout bounds the quorum check.
const (
	dbTimeout           = "10s"
	forwardTimeout      = "15s"
	linearizableTimeout = "5s"
)

// Statement is one parameterised SQL statement. Params are named parameters
// (`:name`); []byte values are sent as JSON byte arrays so SQLite stores them
// as BLOB, never as base64 TEXT (see Blob).
type Statement struct {
	SQL    string
	Params map[string]any
}

// StatementResult is the per-statement entry of the rqlite JSON response.
// rows_affected is omitted by rqlite when zero.
type StatementResult struct {
	Error        string   `json:"error"`
	RowsAffected int64    `json:"rows_affected"`
	Columns      []string `json:"columns"`
	Types        []string `json:"types"`
	Values       [][]any  `json:"values"`
}

// Response is a decoded rqlite HTTP response body.
type Response struct {
	Results   []StatementResult `json:"results"`
	RaftIndex int64             `json:"raft_index"`
	Error     string            `json:"error"`
}

// Err is returned for any rqlite-level failure: a transport error, an HTTP
// status >= 400, or a database error inside an HTTP 200 response. rqlite
// signals database errors inside a 200 body, so status alone is never enough
// (https://rqlite.io/docs/api/api/#handling-errors).
type Err struct {
	Endpoint string
	Status   int
	Message  string
}

func (e *Err) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("rqlite %s: HTTP %d: %s", e.Endpoint, e.Status, e.Message)
	}
	return fmt.Sprintf("rqlite %s: %s", e.Endpoint, e.Message)
}

// Transient reports whether retrying the request is meaningful: a transport
// failure or a 4xx/5xx response by rqlite. rqlite also relays a failure to
// reach the leader as **HTTP 200** with a connection error in the body
// (verified against v10.3.5: `{"results":[],"error":"dial tcp
// <leader>: connect: connection refused"}`, with the same text sometimes in
// `results[i].error`), so the status alone is not enough to classify an
// error — the message has to be inspected. Retrying is always safe for this
// adapter because every mutation carries a request id and is de-duplicated.
func (e *Err) Transient() bool {
	if e.Status == 0 || e.Status >= 500 {
		return true
	}
	for _, marker := range transientMarkers {
		if strings.Contains(e.Message, marker) {
			return true
		}
	}
	return false
}

// transientMarkers are the connectivity and leadership failures rqlite
// reports. A database error (a SQLite message such as "no such table") is
// deliberately absent: those are permanent and must surface to the caller.
var transientMarkers = []string{
	"connect: connection refused",
	"connect: connection reset",
	"connection reset by peer",
	"broken pipe",
	"i/o timeout",
	"context deadline exceeded",
	"transport is closing",
	"no leader",
	"leader not found",
}

// Blob converts raw bytes into the JSON form rqlite binds as a SQLite BLOB.
// A Go []byte would marshal to base64 and rqlite would bind it as TEXT, so
// keys and values must be sent as byte arrays.
func Blob(b []byte) []int {
	if b == nil {
		return nil
	}
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

// Client talks to one or more rqlite nodes. Every request is sent to a single
// endpoint; a transient failure fails over to the next node, which is what
// keeps an adapter client working across a leader change.
type Client struct {
	endpoints []string
	http      *http.Client

	next     atomic.Uint64
	requests atomic.Int64
	reads    atomic.Int64
	weakRead atomic.Int64
}

// NewClient builds a client for the given HTTP endpoints.
func NewClient(endpoints ...string) *Client {
	if len(endpoints) == 0 {
		panic("rqlitecompat: NewClient needs at least one endpoint")
	}
	return &Client{
		endpoints: endpoints,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SetTransport replaces the HTTP transport. Tests use it to drop a response
// after rqlite has already committed the transaction.
func (c *Client) SetTransport(rt http.RoundTripper) {
	c.http.Transport = rt
}

// Requests is the number of HTTP requests this client issued.
func (c *Client) Requests() int64 { return c.requests.Load() }

// Reads reports the number of read requests and how many of them were sent
// without an explicit read-consistency level. rqlite's default is `weak`,
// which is not linearizable, so the adapter must always pass a level.
func (c *Client) Reads() (total, withoutLevel int64) {
	return c.reads.Load(), c.weakRead.Load()
}

// Write POSTs statements to /db/request with `transaction`, so the whole list
// is applied as one SQLite transaction carried by a single Raft log entry.
func (c *Client) Write(ctx context.Context, stmts ...Statement) (Response, error) {
	path := "/db/request?transaction&blob_array&db_timeout=" + dbTimeout + "&timeout=" + forwardTimeout
	return c.post(ctx, path, stmts)
}

// Read runs SELECT statements with an explicit `linearizable` level: the node
// records the Raft commit index, confirms leadership with a quorum and only
// then reads the local database, so the result cannot be stale.
func (c *Client) Read(ctx context.Context, stmts ...Statement) (Response, error) {
	path := "/db/query?level=linearizable&linearizable_timeout=" + linearizableTimeout +
		"&blob_array&db_timeout=" + dbTimeout + "&timeout=" + forwardTimeout
	c.reads.Add(1)
	return c.post(ctx, path, stmts)
}

func (c *Client) post(ctx context.Context, path string, stmts []Statement) (Response, error) {
	payload, err := json.Marshal(encodeStatements(stmts))
	if err != nil {
		return Response{}, fmt.Errorf("encode statements: %w", err)
	}

	n := len(c.endpoints)
	start := int(c.next.Add(1)-1) % n
	var lastErr error
	for i := 0; i < n; i++ {
		endpoint := c.endpoints[(start+i)%n]
		resp, err := c.postOne(ctx, endpoint, path, payload)
		if err == nil {
			return resp, nil
		}
		var rerr *Err
		if errors.As(err, &rerr) && !rerr.Transient() {
			return Response{}, err
		}
		lastErr = err
	}
	return Response{}, lastErr
}

func (c *Client) postOne(ctx context.Context, endpoint, path string, payload []byte) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	c.requests.Add(1)
	httpResp, err := c.http.Do(req)
	if err != nil {
		return Response{}, &Err{Endpoint: endpoint, Message: err.Error()}
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return Response{}, &Err{Endpoint: endpoint, Status: httpResp.StatusCode, Message: err.Error()}
	}

	if httpResp.StatusCode >= 400 {
		return Response{}, &Err{Endpoint: endpoint, Status: httpResp.StatusCode, Message: strings.TrimSpace(string(body))}
	}

	var resp Response
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		return Response{}, &Err{Endpoint: endpoint, Status: httpResp.StatusCode, Message: "decode response: " + err.Error()}
	}
	if resp.Error != "" {
		return Response{}, &Err{Endpoint: endpoint, Status: httpResp.StatusCode, Message: resp.Error}
	}
	for i, r := range resp.Results {
		if r.Error != "" {
			return Response{}, &Err{
				Endpoint: endpoint,
				Status:   httpResp.StatusCode,
				Message:  fmt.Sprintf("statement %d: %s", i, r.Error),
			}
		}
	}
	return resp, nil
}

// NodeStatus is the subset of /status the harness needs to observe elections
// and durability progress. Field paths follow the real v10.3.5 response:
// store.node_id, store.leader.node_id, store.db_applied_index,
// store.raft.applied_index and build.version.
type NodeStatus struct {
	Store struct {
		NodeID string `json:"node_id"`
		Leader struct {
			NodeID string `json:"node_id"`
			Addr   string `json:"addr"`
		} `json:"leader"`
		DBAppliedIndex int64 `json:"db_applied_index"`
		Raft           struct {
			AppliedIndex int64 `json:"applied_index"`
		} `json:"raft"`
	} `json:"store"`
	Build struct {
		Version string `json:"version"`
	} `json:"build"`
}

// Status fetches /status from one node.
func (c *Client) Status(ctx context.Context, endpoint string) (NodeStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/status", nil)
	if err != nil {
		return NodeStatus{}, err
	}
	c.requests.Add(1)
	resp, err := c.http.Do(req)
	if err != nil {
		return NodeStatus{}, &Err{Endpoint: endpoint, Message: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return NodeStatus{}, &Err{Endpoint: endpoint, Status: resp.StatusCode, Message: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return NodeStatus{}, &Err{Endpoint: endpoint, Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	var st NodeStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return NodeStatus{}, &Err{Endpoint: endpoint, Status: resp.StatusCode, Message: "decode status: " + err.Error()}
	}
	return st, nil
}

// Ready reports whether a node answers /readyz with 200. rqlite checks node,
// leader, store and db readiness there.
func (c *Client) Ready(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/readyz", nil)
	if err != nil {
		return err
	}
	c.requests.Add(1)
	resp, err := c.http.Do(req)
	if err != nil {
		return &Err{Endpoint: endpoint, Message: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &Err{Endpoint: endpoint, Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	return nil
}

func encodeStatements(stmts []Statement) []any {
	out := make([]any, 0, len(stmts))
	for _, s := range stmts {
		if len(s.Params) == 0 {
			out = append(out, s.SQL)
			continue
		}
		out = append(out, []any{s.SQL, s.Params})
	}
	return out
}
