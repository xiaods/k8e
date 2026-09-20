package etcdrobustness

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fakeReader(states map[string]State) Reader {
	return func(_ context.Context, key string) (State, error) {
		return states[key], nil
	}
}

func ackedPut(key, value string, revision int64) Record {
	return Record{
		OperationID: key,
		Kind:        KindPut,
		Key:         key,
		ValueSHA256: HashValue(value),
		Outcome:     OutcomeAcknowledged,
		Revision:    revision,
	}
}

func TestVerifyPreservesAcknowledgedWrite(t *testing.T) {
	history := []Record{ackedPut("k", "v", 3)}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("v"), Revision: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("violations: %v", report.Violations)
	}
	if report.Checked != 1 {
		t.Fatalf("checked = %d", report.Checked)
	}
}

func TestVerifyDetectsLostAcknowledgedWrite(t *testing.T) {
	history := []Record{ackedPut("k", "v", 3)}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{}))
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("a missing acknowledged write must violate the oracle")
	}
}

func TestVerifyExplainsUnknownThatCommitted(t *testing.T) {
	history := []Record{
		ackedPut("k", "v", 3),
		{
			OperationID: "unknown",
			Kind:        KindPut,
			Key:         "k",
			ValueSHA256: HashValue("w"),
			Outcome:     OutcomeUnknown,
		},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("w"), Revision: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("an unknown write that did commit must be explainable: %v", report.Violations)
	}
	if len(report.UnknownIntents) != 1 || report.UnknownIntents[0] != "unknown" {
		t.Fatalf("unknown intents = %v", report.UnknownIntents)
	}
}

func TestVerifyExplainsUnknownThatDidNotCommit(t *testing.T) {
	history := []Record{
		ackedPut("k", "v", 3),
		{
			OperationID: "unknown",
			Kind:        KindPut,
			Key:         "k",
			ValueSHA256: HashValue("w"),
			Outcome:     OutcomeUnknown,
		},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("v"), Revision: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("an unknown write that did not commit must be explainable: %v", report.Violations)
	}
}

func TestVerifyRejectsUnexplainedState(t *testing.T) {
	history := []Record{
		ackedPut("k", "v", 3),
		{OperationID: "u", Kind: KindPut, Key: "k", ValueSHA256: HashValue("w"), Outcome: OutcomeUnknown},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("tampered"), Revision: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("a value nobody acknowledged must violate the oracle")
	}
	// The violation must name every state that would have been acceptable,
	// including the in-flight write's, so the report is actionable.
	joint := strings.Join(report.Violations, "\n")
	if !strings.Contains(joint, HashValue("v")) || !strings.Contains(joint, HashValue("w")) {
		t.Fatalf("violation does not list both allowed states: %v", report.Violations)
	}
}

func TestVerifyDeleteIsNotResurrected(t *testing.T) {
	history := []Record{
		ackedPut("k", "v", 3),
		{OperationID: "d", Kind: KindDelete, Key: "k", Outcome: OutcomeAcknowledged, Revision: 4},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("v"), Revision: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("an acknowledged delete must not be silently resurrected")
	}

	report, err = Verify(context.Background(), history, fakeReader(map[string]State{}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("absent state after an acknowledged delete must pass: %v", report.Violations)
	}
}

func TestVerifyAllowsUnknownDelete(t *testing.T) {
	history := []Record{
		ackedPut("k", "v", 3),
		{OperationID: "d", Kind: KindDelete, Key: "k", Outcome: OutcomeUnknown},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("an unknown delete that did commit must be explainable: %v", report.Violations)
	}
}

func TestVerifyIgnoresRejectedOperations(t *testing.T) {
	history := []Record{
		ackedPut("k", "v", 3),
		{OperationID: "r", Kind: KindPut, Key: "k", ValueSHA256: HashValue("w"), Outcome: OutcomeRejected},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("v"), Revision: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("rejected writes must not constrain the oracle: %v", report.Violations)
	}
}

func TestVerifyIgnoresUnknownOnlyKeyThatNeverCommitted(t *testing.T) {
	history := []Record{
		{OperationID: "u", Kind: KindPut, Key: "k", ValueSHA256: HashValue("w"), Outcome: OutcomeUnknown},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("a key that only ever had an unknown write may be absent: %v", report.Violations)
	}
}

func TestVerifyReturnsReaderErrors(t *testing.T) {
	reader := func(context.Context, string) (State, error) { return State{}, errors.New("connection refused") }
	_, err := Verify(context.Background(), []Record{ackedPut("k", "v", 3)}, reader)
	if err == nil {
		t.Fatal("a reader failure must surface as an error")
	}
}

// TestVerifyRejectsUnknownCASWinnerAfterAcknowledgedCAS is the counterexample
// the phase-1 oracle must not accept: one CAS at expected revision 3 was
// acknowledged at revision 4, and a second CAS of the same expected revision is
// unknown. Only one compare can win, so the unknown CAS cannot have committed
// and its payload must not be reported as a correct recovery.
func TestVerifyRejectsUnknownCASWinnerAfterAcknowledgedCAS(t *testing.T) {
	history := []Record{
		{
			OperationID:       "acked",
			Kind:              KindCAS,
			Key:               "k",
			ValueSHA256:       HashValue("a"),
			ExpectValueSHA256: HashValue("seed"),
			ExpectRevision:    3,
			Outcome:           OutcomeAcknowledged,
			Revision:          4,
		},
		{
			OperationID:       "unknown",
			Kind:              KindCAS,
			Key:               "k",
			ValueSHA256:       HashValue("b"),
			ExpectValueSHA256: HashValue("seed"),
			ExpectRevision:    3,
			Outcome:           OutcomeUnknown,
		},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("b"), Revision: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("an unknown CAS that lost the race must not be accepted as the recovered state")
	}
}

// TestVerifyRejectsUnknownCASBehindLaterAcknowledgedMutation covers the general
// rule: once an acknowledged mutation has moved the key past the expected
// revision, the unknown CAS either committed before it (and was overwritten) or
// read the newer revision and failed, so it can never be the recovered state.
func TestVerifyRejectsUnknownCASBehindLaterAcknowledgedMutation(t *testing.T) {
	history := []Record{
		ackedPut("k", "older", 3),
		ackedPut("k", "newer", 5),
		{OperationID: "unknown", Kind: KindCAS, Key: "k", ValueSHA256: HashValue("late"), ExpectRevision: 3, Outcome: OutcomeUnknown},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("late"), Revision: 6},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("an unknown CAS behind a later acknowledged mutation must not be explainable")
	}
}

// TestVerifyExplainsUnknownCASThatCommitted keeps the positive side: an unknown
// CAS at the key's last acknowledged revision may really have committed, so its
// effect must stay explainable.
func TestVerifyExplainsUnknownCASThatCommitted(t *testing.T) {
	history := []Record{
		ackedPut("k", "seed", 3),
		{
			OperationID:       "unknown",
			Kind:              KindCAS,
			Key:               "k",
			ValueSHA256:       HashValue("winner"),
			ExpectValueSHA256: HashValue("seed"),
			ExpectRevision:    3,
			Outcome:           OutcomeUnknown,
		},
	}
	report, err := Verify(context.Background(), history, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("winner"), Revision: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("an unknown CAS that did commit must be explainable: %v", report.Violations)
	}
}

func TestVerifyAtMostOneCASWinner(t *testing.T) {
	winner := Record{OperationID: "a", Kind: KindCAS, Key: "k", ValueSHA256: HashValue("w"), ExpectRevision: 3, Outcome: OutcomeAcknowledged, Revision: 4}
	report, err := Verify(context.Background(), []Record{winner}, fakeReader(map[string]State{
		"k": {Exists: true, ValueSHA256: HashValue("w"), Revision: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("a single CAS winner must pass: %v", report.Violations)
	}

	second := winner
	second.OperationID = "b"
	second.ValueSHA256 = HashValue("x")
	violations := VerifyAtMostOneCASWinner([]Record{winner, second})
	if len(violations) != 1 {
		t.Fatalf("two winners for the same revision must violate: %v", violations)
	}

	second.ExpectRevision = 4
	if violations := VerifyAtMostOneCASWinner([]Record{winner, second}); len(violations) != 0 {
		t.Fatalf("a later expected revision is a different CAS target: %v", violations)
	}
}

func TestMaxAcknowledgedRevision(t *testing.T) {
	history := []Record{
		ackedPut("a", "v", 4),
		{OperationID: "u", Kind: KindPut, Key: "b", Outcome: OutcomeUnknown, Revision: 99},
		{OperationID: "r", Kind: KindPut, Key: "c", Outcome: OutcomeRejected, Revision: 100},
		ackedPut("d", "v", 6),
	}
	if got := MaxAcknowledgedRevision(history); got != 6 {
		t.Fatalf("max acknowledged revision = %d", got)
	}
}

func TestVerifyRevisionContinuity(t *testing.T) {
	if violations := VerifyRevisionContinuity(10, 10); len(violations) != 0 {
		t.Fatalf("equal revision must pass: %v", violations)
	}
	if violations := VerifyRevisionContinuity(11, 10); len(violations) != 0 {
		t.Fatalf("advanced revision must pass: %v", violations)
	}
	violations := VerifyRevisionContinuity(3, 10)
	if len(violations) != 1 {
		t.Fatalf("a rollback must violate: %v", violations)
	}
}
