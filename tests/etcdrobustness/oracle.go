package etcdrobustness

import (
	"context"
	"fmt"
	"sort"
)

// State is the state of one key observed after recovery.
type State struct {
	Exists      bool
	ValueSHA256 string
	Revision    int64
}

// Reader reads the recovered state of one key. The empty hash means "absent",
// so it doubles as the payload fingerprint compared against the history.
type Reader func(ctx context.Context, key string) (State, error)

// Report is the outcome of one oracle run.
type Report struct {
	Checked        int
	UnknownIntents []string
	Violations     []string
}

// OK reports whether the oracle found no violation.
func (r *Report) OK() bool { return len(r.Violations) == 0 }

// Verify checks the phase-1 correctness oracle against the observed state:
//
//   - a key's state must be explainable by the acknowledged history, allowing
//     for an unknown operation that may have committed before the process died;
//   - at most one concurrent CAS may win for the same key and expected revision.
//
// Unknown outcomes are not failures by themselves: the mission is to explain
// them, which the key-state check does by including their possible effects.
func Verify(ctx context.Context, history []Record, read Reader) (*Report, error) {
	report := &Report{}
	for _, record := range history {
		if record.Outcome == OutcomeUnknown {
			report.UnknownIntents = append(report.UnknownIntents, record.OperationID)
		}
	}
	report.Violations = append(report.Violations, VerifyAtMostOneCASWinner(history)...)
	if err := verifyKeyStates(ctx, history, read, report); err != nil {
		return report, err
	}
	sort.Strings(report.Violations)
	return report, nil
}

func verifyKeyStates(ctx context.Context, history []Record, read Reader, report *Report) error {
	byKey := make(map[string][]Record)
	order := make([]string, 0, len(history))
	for _, record := range history {
		if record.Outcome != OutcomeAcknowledged && record.Outcome != OutcomeUnknown {
			continue
		}
		if _, seen := byKey[record.Key]; !seen {
			order = append(order, record.Key)
		}
		byKey[record.Key] = append(byKey[record.Key], record)
	}

	for _, key := range order {
		allowed := allowedStates(byKey[key])
		state, err := read(ctx, key)
		if err != nil {
			return fmt.Errorf("read recovered state of %q: %w", key, err)
		}
		report.Checked++
		if _, ok := allowed[state.ValueSHA256]; !ok {
			report.Violations = append(report.Violations, fmt.Sprintf(
				"key %q is not explainable: observed=%s allowed=%s",
				key, describeHash(state.ValueSHA256), describeAllowed(allowed)))
		}
	}
	return nil
}

// allowedStates returns the payload hashes the key may hold after recovery.
//
// The baseline is the effect of the highest-revision acknowledged mutation (or
// "absent" when nothing was acknowledged). Every unknown operation on the key
// adds its own possible effect, because a request that was in flight when the
// process died may have been committed with a revision after the baseline.
//
// An unknown CAS is not an unconditional put. Its effect can only be the
// recovered state if the compare could still succeed once every acknowledged
// mutation of the key has been applied: the baseline is the last acknowledged
// state, so a CAS whose expected revision is older than it either lost the race
// (an acknowledged mutation overwrote it) or read the newer revision and
// failed. Allowing its payload anyway would report an impossible winner — an
// acknowledged CAS and a second CAS of the same expected revision both having
// committed — as a correct recovery.
func allowedStates(records []Record) map[string]struct{} {
	allowed := make(map[string]struct{})
	var baseline *Record
	for i := range records {
		record := &records[i]
		if record.Outcome != OutcomeAcknowledged {
			continue
		}
		if baseline == nil || record.Revision > baseline.Revision {
			baseline = record
		}
	}
	if baseline == nil {
		allowed[""] = struct{}{}
	} else {
		allowed[effectHash(*baseline)] = struct{}{}
	}
	for i := range records {
		record := &records[i]
		if record.Outcome != OutcomeUnknown {
			continue
		}
		if record.Kind == KindCAS && baseline != nil && baseline.Revision > record.ExpectRevision {
			continue
		}
		allowed[effectHash(*record)] = struct{}{}
	}
	return allowed
}

// effectHash is the payload hash a record leaves behind; a delete leaves the
// key absent, which is the empty hash.
func effectHash(record Record) string {
	if record.Kind == KindDelete {
		return ""
	}
	return record.ValueSHA256
}

func describeHash(hash string) string {
	if hash == "" {
		return "absent"
	}
	return hash
}

func describeAllowed(allowed map[string]struct{}) string {
	values := make([]string, 0, len(allowed))
	for hash := range allowed {
		values = append(values, describeHash(hash))
	}
	sort.Strings(values)
	out := ""
	for i, value := range values {
		if i > 0 {
			out += "|"
		}
		out += value
	}
	return out
}

// VerifyAtMostOneCASWinner checks that no two acknowledged CAS operations
// targeted the same key and expected revision: etcd bumps the revision on the
// first winner, so a second success would mean the CAS was not applied.
func VerifyAtMostOneCASWinner(history []Record) []string {
	winners := make(map[string]int)
	for _, record := range history {
		if record.Kind != KindCAS || record.Outcome != OutcomeAcknowledged {
			continue
		}
		winners[fmt.Sprintf("%s@%d", record.Key, record.ExpectRevision)]++
	}
	violations := make([]string, 0)
	for target, count := range winners {
		if count > 1 {
			violations = append(violations, fmt.Sprintf("CAS %s was acknowledged %d times", target, count))
		}
	}
	sort.Strings(violations)
	return violations
}

// MaxAcknowledgedRevision is the highest revision the workload saw acknowledged.
// The recovered store must not report a revision below it.
func MaxAcknowledgedRevision(history []Record) int64 {
	var highest int64
	for _, record := range history {
		if record.Outcome == OutcomeAcknowledged && record.Revision > highest {
			highest = record.Revision
		}
	}
	return highest
}

// VerifyRevisionContinuity checks that the recovered store did not roll back
// below the last acknowledged revision, which would prove a lost commit or a
// silently recreated empty cluster.
func VerifyRevisionContinuity(observed, floor int64) []string {
	if observed < floor {
		return []string{fmt.Sprintf("recovered revision %d is below the last acknowledged revision %d", observed, floor)}
	}
	return nil
}
