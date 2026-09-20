package etcdrobustness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow(t *testing.T) func() time.Time {
	t.Helper()
	base := time.Date(2026, 9, 20, 2, 53, 0, 0, time.UTC)
	return func() time.Time { return base }
}

func TestHashValueIsStableAndDistinct(t *testing.T) {
	// The pinned digest is the contract: the fingerprint is the hex SHA-256 of
	// the value, so it is both reproducible and comparable to a state read back
	// from the store.
	const sha256OfA = "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	if got := HashValue("a"); got != sha256OfA {
		t.Fatalf("HashValue(\"a\") = %s, want %s", got, sha256OfA)
	}
	if HashValue("a") == HashValue("b") {
		t.Fatal("HashValue collides on distinct values")
	}
	if HashValue("") == "" {
		t.Fatal("HashValue of the empty string must not be the absent sentinel")
	}
}

func TestRecorderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	recorder := mustRecorder(t, path, fixedNow(t))
	recordEveryOutcome(t, recorder)

	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}

	history := mustLoadHistory(t, path)
	if len(history) != 4 {
		t.Fatalf("want 4 operations, got %d", len(history))
	}
	assertRoundTripRecords(t, history)
}

// recordEveryOutcome writes one operation per terminal outcome plus a pending
// intent, which is the history a strong kill leaves behind.
func recordEveryOutcome(t *testing.T, recorder *Recorder) {
	t.Helper()
	put := mustBegin(t, recorder, Operation{Kind: KindPut, Key: "k", Value: "v", Endpoint: "http://127.0.0.1:1"})
	must(t, put.Acknowledge(7))

	deleteAttempt := mustBegin(t, recorder, Operation{Kind: KindDelete, Key: "k"})
	must(t, deleteAttempt.Reject(errors.New("boom")))

	cas := mustBegin(t, recorder, Operation{Kind: KindCAS, Key: "k", Value: "w", ExpectValue: "v", ExpectRevision: 7})
	must(t, cas.Unknown(errors.New("timeout")))

	// A pending intent without a terminal record is what a strong kill leaves.
	mustBegin(t, recorder, Operation{Kind: KindPut, Key: "k", Value: "z"})
}

// assertRoundTripRecords checks the loaded history against what was recorded.
func assertRoundTripRecords(t *testing.T, history []Record) {
	t.Helper()
	for _, record := range history {
		if record.OperationID == "" {
			t.Fatal("record without operation id")
		}
		if record.StartedAt.IsZero() {
			t.Fatal("record without start time")
		}
	}
	if history[0].Outcome != OutcomeAcknowledged || history[0].Revision != 7 {
		t.Fatalf("put record = %+v", history[0])
	}
	if history[0].ValueSHA256 != HashValue("v") {
		t.Fatalf("put value hash = %q", history[0].ValueSHA256)
	}
	if history[1].Outcome != OutcomeRejected || !strings.Contains(history[1].Error, "boom") {
		t.Fatalf("delete record = %+v", history[1])
	}
	if history[2].Outcome != OutcomeUnknown || history[2].ExpectRevision != 7 {
		t.Fatalf("cas record = %+v", history[2])
	}
	if history[2].ExpectValueSHA256 != HashValue("v") {
		t.Fatalf("cas expectation hash = %q", history[2].ExpectValueSHA256)
	}
	if history[3].Outcome != OutcomeUnknown {
		t.Fatalf("pending record must replay as unknown, got %+v", history[3])
	}
}

// mustRecorder opens an operation history or fails the test.
func mustRecorder(t *testing.T, path string, now func() time.Time) *Recorder {
	t.Helper()
	recorder, err := newRecorder(path, now)
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

// mustBegin starts one recorded operation or fails the test.
func mustBegin(t *testing.T, recorder *Recorder, op Operation) *Attempt {
	t.Helper()
	attempt, err := recorder.Begin(op)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

// must fails the test when a recorded history operation did not succeed.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecorderRejectsDoubleFinish(t *testing.T) {
	recorder, err := newRecorder(filepath.Join(t.TempDir(), "h.jsonl"), fixedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	attempt, err := recorder.Begin(Operation{Kind: KindPut, Key: "k", Value: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.Acknowledge(1); err != nil {
		t.Fatal(err)
	}
	if err := attempt.Acknowledge(2); err == nil {
		t.Fatal("a second terminal record must be rejected")
	}
}

func TestRecorderReportsBrokenHistoryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	recorder, err := newRecorder(path, fixedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	// Closing the descriptor behind the recorder's back is what a broken
	// filesystem or an exhausted disk looks like to the caller.
	if err := recorder.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Begin(Operation{Kind: KindPut, Key: "k", Value: "v"}); err == nil {
		t.Fatal("a write to a broken history file must be reported")
	}
}

func TestRecorderReportsSyncFailure(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()

	// A pipe accepts the record and rejects the fsync.
	recorder := &Recorder{file: writeEnd, now: fixedNow(t)}
	if _, err := recorder.Begin(Operation{Kind: KindPut, Key: "k", Value: "v"}); err == nil {
		t.Fatal("a failed fsync must be reported")
	}
}

func TestRecorderBeginAfterClose(t *testing.T) {
	recorder, err := newRecorder(filepath.Join(t.TempDir(), "h.jsonl"), fixedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Begin(Operation{Kind: KindPut, Key: "k", Value: "v"}); err == nil {
		t.Fatal("Begin after Close must fail")
	}
}

func TestNewRecorderError(t *testing.T) {
	if _, err := NewRecorder(filepath.Join(t.TempDir(), "missing", "h.jsonl")); err == nil {
		t.Fatal("opening a history in a missing directory must fail")
	}
}

func TestLoadHistoryErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadHistory(filepath.Join(dir, "absent.jsonl")); err == nil {
		t.Fatal("loading an absent history must fail")
	}

	cases := map[string]string{
		"malformed.jsonl": `{"operation_id":`,
		"noid.jsonl":      `{"kind":"put","key":"k"}`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadHistory(path); err == nil {
			t.Fatalf("%s must fail to load", name)
		}
	}
}

func TestLoadHistorySkipsBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	body := "\n{\"operation_id\":\"1\",\"kind\":\"put\",\"key\":\"k\",\"outcome\":\"acknowledged\",\"revision\":3}\n\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	history, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Revision != 3 {
		t.Fatalf("history = %+v", history)
	}
}

func TestLoadHistoryKeepsLastRecordPerOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	body := `{"operation_id":"1","kind":"put","key":"k","outcome":"pending"}
{"operation_id":"2","kind":"put","key":"k","outcome":"pending"}
{"operation_id":"1","kind":"put","key":"k","outcome":"acknowledged","revision":9}
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	history, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("want 2 operations, got %d", len(history))
	}
	if history[0].Outcome != OutcomeAcknowledged || history[0].Revision != 9 {
		t.Fatalf("operation 1 = %+v", history[0])
	}
	if history[1].Outcome != OutcomeUnknown {
		t.Fatalf("operation 2 = %+v", history[1])
	}
}
