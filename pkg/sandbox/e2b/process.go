package e2b

import (
	"sync"
)

// ProcessTable is the daemon-side process table behind the E2B process
// surface. E2B's defining behavior — `background: true` is the same wire as a
// foreground run, `disconnect()` never kills, `connect(pid)` reattaches —
// means a process's lifetime must be decoupled from any HTTP response's.
// This table is that decoupling: envd streams are subscribers that come and
// go; the process lives here until it exits or its sandbox dies.
//
// Deliberately not persisted: a daemon restart empties the table — the
// in-container processes it tracked become orphans that the sandbox's own
// death reaps. Honest and documented, and better than E2B, whose registry
// does not even survive pause/resume.
type ProcessTable struct {
	mu      sync.Mutex
	nextPid int
	records map[int]*ProcessRecord
}

// ProcessConfig is the process config as the SDK sent it, echoed back
// verbatim by List.
type ProcessConfig struct {
	Cmd  string
	Args []string
	Envs map[string]string
	Cwd  string
}

// OutputChannel is one of the two wire channels a subscriber may receive.
type OutputChannel string

const (
	ChannelStdout OutputChannel = "stdout"
	ChannelStderr OutputChannel = "stderr"
)

// ProcessEnd is how a process ends: an exit (with code) or an error.
type ProcessEnd struct {
	ExitCode int
	Err      error
}

// ProcessSubscriber receives output chunks and the process ending.
type ProcessSubscriber interface {
	OnOutput(channel OutputChannel, data []byte)
	OnEnd(end ProcessEnd)
}

// ProcessRecord is a living process's table entry.
type ProcessRecord struct {
	PID       int
	SandboxID string
	Config    ProcessConfig
	Subs      []ProcessSubscriber
	done      chan struct{}
	// GuestPID is the in-sandbox pid as reported by sandboxd's /exec/stream
	// (the first SSE frame carries it). It is what /exec/stdin and
	// /exec/signal address; nil until the stream opens. Only the table's
	// SetGuestPID/GuestPID methods touch it, under t.mu (the stream goroutine
	// writes it, request goroutines read it).
	GuestPID *int
}

// NewProcessTable creates an empty process table.
func NewProcessTable() *ProcessTable {
	return &ProcessTable{nextPid: 1000, records: map[int]*ProcessRecord{}}
}

// Start registers a process with a fresh pid and returns the record. The
// caller is responsible for running the actual exec and finalizing via End.
func (t *ProcessTable) Start(sandboxID string, cfg ProcessConfig) *ProcessRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec := &ProcessRecord{
		PID:       t.nextPid,
		SandboxID: sandboxID,
		Config:    cfg,
		done:      make(chan struct{}),
	}
	t.nextPid++
	t.records[rec.PID] = rec
	return rec
}

// Get returns a living process for a sandbox, or nil. A pid from another
// sandbox is invisible.
func (t *ProcessTable) Get(sandboxID string, pid int) *ProcessRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[pid]
	if !ok || rec.SandboxID != sandboxID {
		return nil
	}
	return rec
}

// List returns the living processes of a sandbox.
func (t *ProcessTable) List(sandboxID string) []*ProcessRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*ProcessRecord
	for _, rec := range t.records {
		if rec.SandboxID == sandboxID {
			out = append(out, rec)
		}
	}
	return out
}

// SetGuestPID records the in-sandbox pid once the stream reports it. The
// stream goroutine writes it while request goroutines may read it via
// GuestPID, so both go through the table lock.
func (t *ProcessTable) SetGuestPID(pid, guest int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.records[pid]; ok {
		rec.GuestPID = &guest
	}
}

// GuestPID returns the recorded in-sandbox pid and whether it is known yet.
func (t *ProcessTable) GuestPID(pid int) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[pid]
	if !ok || rec.GuestPID == nil {
		return 0, false
	}
	return *rec.GuestPID, true
}

// Subscribe adds a subscriber. No replay: a late subscriber sees output from
// now on, same as real envd.
func (t *ProcessTable) Subscribe(pid int, sub ProcessSubscriber) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[pid]
	if !ok {
		return false
	}
	rec.Subs = append(rec.Subs, sub)
	return true
}

// Unsubscribe removes a subscriber.
func (t *ProcessTable) Unsubscribe(pid int, sub ProcessSubscriber) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[pid]
	if !ok {
		return
	}
	for i, s := range rec.Subs {
		if s == sub {
			rec.Subs = append(rec.Subs[:i], rec.Subs[i+1:]...)
			break
		}
	}
}

// ReplaceSubscriber swaps one subscriber for another on a process's stream
// (used to wrap a raw subscriber with a keepalive decorator). No-op if the
// original is not present.
func (t *ProcessTable) ReplaceSubscriber(pid int, old, fresh ProcessSubscriber) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[pid]
	if !ok {
		return
	}
	for i, s := range rec.Subs {
		if s == old {
			rec.Subs[i] = fresh
			return
		}
	}
}

// Broadcast delivers a chunk to every subscriber. With zero subscribers the
// chunk is dropped — a background process nobody watches drains and drops,
// it never wedges.
func (t *ProcessTable) Broadcast(pid int, channel OutputChannel, data []byte) {
	t.mu.Lock()
	rec, ok := t.records[pid]
	if !ok {
		t.mu.Unlock()
		return
	}
	subs := append([]ProcessSubscriber(nil), rec.Subs...)
	t.mu.Unlock()
	for _, sub := range subs {
		sub.OnOutput(channel, data)
	}
}

// End finalizes a process: broadcasts the ending and removes the entry.
func (t *ProcessTable) End(pid int, end ProcessEnd) {
	t.mu.Lock()
	rec, ok := t.records[pid]
	if !ok {
		t.mu.Unlock()
		return
	}
	delete(t.records, pid)
	subs := rec.Subs
	rec.Subs = nil
	close(rec.done)
	t.mu.Unlock()
	for _, sub := range subs {
		sub.OnEnd(end)
	}
}

// Done returns a channel closed when the process finalizes (used to gate a
// Connect subscriber's stream end).
func (t *ProcessTable) Done(pid int) (<-chan struct{}, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[pid]
	if !ok {
		return nil, false
	}
	return rec.done, true
}
