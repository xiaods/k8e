// Package etcdrobustness holds the deterministic, in-process layer of the
// embedded-etcd robustness suite described in
// hack/e2e/etcd-robustness/DESIGN.md.
//
// It is test support: nothing in the shipped binaries imports it. The package
// provides the three building blocks the design calls for:
//
//   - an external operation history that lives outside the etcd data
//     directory, so a fault cannot take the record of what happened with it;
//   - the correctness oracle that decides whether the state observed after a
//     fault is explainable by the acknowledged history;
//   - a small helper that starts a single embedded etcd node with a fixed data
//     directory, so graceful restarts and strong kills can reuse it.
//
// The Docker/VM layers (K8E container restarts, disk faults, multi-member
// failures) build on the same history and oracle; see the design document.
package etcdrobustness

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Outcome is the terminal classification of one operation.
//
// A timeout or a connection break is deliberately not a failure: the request
// may have been committed by etcd after the client stopped waiting. It is
// OutcomeUnknown until the recovery oracle has seen the state and explained it.
type Outcome string

const (
	// OutcomePending is written before the request is issued. A pending record
	// whose process died is replayed as OutcomeUnknown by LoadHistory.
	OutcomePending Outcome = "pending"
	// OutcomeAcknowledged means the server returned a response (success).
	OutcomeAcknowledged Outcome = "acknowledged"
	// OutcomeRejected means the server explicitly refused the request.
	OutcomeRejected Outcome = "rejected"
	// OutcomeUnknown means the client never learned the outcome.
	OutcomeUnknown Outcome = "unknown"
)

// Kind is the mutating operation a Record describes.
type Kind string

const (
	KindPut    Kind = "put"
	KindDelete Kind = "delete"
	KindCAS    Kind = "cas"
)

// Record is one operation in the external history. Intent records carry the
// payload hash, so the oracle can still reason about an operation that was in
// flight when the process was killed.
type Record struct {
	OperationID       string    `json:"operation_id"`
	Kind              Kind      `json:"kind"`
	Key               string    `json:"key"`
	ValueSHA256       string    `json:"value_sha256,omitempty"`
	ExpectValueSHA256 string    `json:"expect_value_sha256,omitempty"`
	ExpectRevision    int64     `json:"expect_revision,omitempty"`
	Endpoint          string    `json:"endpoint,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at,omitempty"`
	Outcome           Outcome   `json:"outcome"`
	Revision          int64     `json:"revision,omitempty"`
	Error             string    `json:"error,omitempty"`
}

// HashValue is the payload fingerprint used everywhere in the history so that
// a value is compared without ever storing a secret verbatim.
func HashValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Operation is the caller-supplied description of one mutation. The payload is
// hashed at Begin time, before the request is sent.
type Operation struct {
	Kind           Kind
	Key            string
	Value          string
	ExpectValue    string
	ExpectRevision int64
	Endpoint       string
}

// Recorder appends the operation history to a file with a fsync per record.
//
// It is safe for concurrent use. The file is expected to live outside the etcd
// data directory so the history survives the fault under test.
type Recorder struct {
	mu   sync.Mutex
	file *os.File
	seq  uint64
	now  func() time.Time
}

// NewRecorder opens (or creates) path for appending.
func NewRecorder(path string) (*Recorder, error) {
	return newRecorder(path, time.Now)
}

func newRecorder(path string, now func() time.Time) (*Recorder, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open operation history %s: %w", path, err)
	}
	return &Recorder{file: file, now: now}, nil
}

// Close closes the history file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// Attempt is one in-flight operation. A terminal method must be called exactly
// once; if the process dies before that, the pending record becomes unknown.
type Attempt struct {
	recorder *Recorder
	record   Record
	done     bool
}

// Begin records the intent (including the payload hash) and fsyncs it before
// the caller issues the request.
func (r *Recorder) Begin(op Operation) (*Attempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil, fmt.Errorf("operation history is closed")
	}

	r.seq++
	record := Record{
		OperationID:    r.newOperationID(),
		Kind:           op.Kind,
		Key:            op.Key,
		ExpectRevision: op.ExpectRevision,
		Endpoint:       op.Endpoint,
		StartedAt:      r.now().UTC(),
		Outcome:        OutcomePending,
	}
	if op.Value != "" {
		record.ValueSHA256 = HashValue(op.Value)
	}
	if op.ExpectValue != "" {
		record.ExpectValueSHA256 = HashValue(op.ExpectValue)
	}
	if err := r.append(record); err != nil {
		return nil, err
	}
	return &Attempt{recorder: r, record: record}, nil
}

// newOperationID returns a process-unique id. Callers must hold r.mu.
func (r *Recorder) newOperationID() string {
	return fmt.Sprintf("%d-%d", r.now().UnixNano(), r.seq)
}

// Acknowledge records a successful response and the revision it returned.
func (a *Attempt) Acknowledge(revision int64) error {
	return a.finish(OutcomeAcknowledged, revision, nil)
}

// Reject records an explicit server refusal.
func (a *Attempt) Reject(err error) error {
	return a.finish(OutcomeRejected, 0, err)
}

// Unknown records that the client never learned the outcome.
func (a *Attempt) Unknown(err error) error {
	return a.finish(OutcomeUnknown, 0, err)
}

func (a *Attempt) finish(outcome Outcome, revision int64, err error) error {
	a.recorder.mu.Lock()
	defer a.recorder.mu.Unlock()
	if a.done {
		return fmt.Errorf("operation %s already finished", a.record.OperationID)
	}
	a.done = true
	a.record.Outcome = outcome
	a.record.Revision = revision
	a.record.FinishedAt = a.recorder.now().UTC()
	if err != nil {
		a.record.Error = err.Error()
	}
	return a.recorder.append(a.record)
}

// append writes one record and fsyncs it. Callers must hold r.mu.
func (r *Recorder) append(record Record) error {
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal operation history: %w", err)
	}
	if _, err := r.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write operation history: %w", err)
	}
	if err := r.file.Sync(); err != nil {
		return fmt.Errorf("sync operation history: %w", err)
	}
	return nil
}

// LoadHistory replays an operation history file.
//
// Records are reduced per operation id: the last line wins, and an operation
// whose only line is a pending intent is replayed as OutcomeUnknown, because
// its process died before the client learned anything.
func LoadHistory(path string) ([]Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open operation history %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	latest := make(map[string]Record)
	order := make([]string, 0, 64)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, fmt.Errorf("parse operation history line %q: %w", line, err)
		}
		if record.OperationID == "" {
			return nil, fmt.Errorf("operation history line without operation_id: %q", line)
		}
		if _, seen := latest[record.OperationID]; !seen {
			order = append(order, record.OperationID)
		}
		latest[record.OperationID] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read operation history %s: %w", path, err)
	}

	history := make([]Record, 0, len(order))
	for _, id := range order {
		record := latest[id]
		if record.Outcome == OutcomePending {
			record.Outcome = OutcomeUnknown
			record.Error = "process stopped with the operation in flight"
		}
		history = append(history, record)
	}
	return history, nil
}
