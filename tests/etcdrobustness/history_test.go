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
	if HashValue("a") != HashValue("a") {
		t.Fatal("HashValue is not deterministic")
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
	recorder, err := newRecorder(path, fixedNow(t))
	if err != nil {
		t.Fatal(err)
	}

	put, err := recorder.Begin(Operation{Kind: KindPut, Key: "k", Value: "v", Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := put.Acknowledge(7); err != nil {
		t.Fatal(err)
	}

	deleteAttempt, err := recorder.Begin(Operation{Kind: KindDelete, Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteAttempt.Reject(errors.New("boom")); err != nil {
		t.Fatal(err)
	}

	cas, err := recorder.Begin(Operation{Kind: KindCAS, Key: "k", Value: "w", ExpectValue: "v", ExpectRevision: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := cas.Unknown(errors.New("timeout")); err != nil {
		t.Fatal(err)
	}

	// A pending intent without a terminal record is what a strong kill leaves.
	if _, err := recorder.Begin(Operation{Kind: KindPut, Key: "k", Value: "z"}); err != nil {
		t.Fatal(err)
	}

	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}

	history, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("want 4 operations, got %d", len(history))
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
	for _, record := range history {
		if record.OperationID == "" {
			t.Fatal("record without operation id")
		}
		if record.StartedAt.IsZero() {
			t.Fatal("record without start time")
		}
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
