//go:build tandem_differential

package differential

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

// TestDifferentialStatus covers Maintenance.Status, which the apiserver calls
// on every start from etcd3.New to read the endpoint's version and decide
// whether RequestWatchProgress is supported. While it was unrouted that probe
// failed on every boot, logging at error level and leaving watch-list initial
// events disabled.
//
// Only the fields the two backends can both answer honestly are compared.
// The leader is a numeric member id in etcd and a string in rqlite, and the db
// size measures a bbolt file on one side and a SQLite file on the other, so
// those are asserted for presence rather than for equality.
func TestDifferentialStatus(t *testing.T) {
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, backend := range []*Backend{h.Etcd, h.Tandem} {
		t.Run(backend.Name, func(t *testing.T) {
			// A write first, so raft has an applied index to report.
			if _, err := backend.Client.Put(ctx, "diff/status/key", "v"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			response, err := pb.NewMaintenanceClient(backend.Client.ActiveConnection()).
				Status(ctx, &pb.StatusRequest{})
			if err != nil {
				t.Fatalf("Status: %v", err)
			}

			// The apiserver parses this string and compares it against the
			// 3.4.31 / 3.5.13 thresholds, so an unparseable or absent version
			// silently disables watch progress rather than failing loudly.
			if response.Version == "" {
				t.Fatal("Status reported no version; the apiserver's watch-progress check reads this field")
			}
			if _, err := parseSemver(response.Version); err != nil {
				t.Fatalf("Status version %q does not parse: %v", response.Version, err)
			}
			// Both must agree that some node leads, or the apiserver's
			// RequireLeader watch option would never be satisfiable.
			if response.Leader == 0 {
				t.Error("Status reports no leader on a single healthy node")
			}
			if response.IsLearner {
				t.Error("Status reports a learner on a node that is a voter")
			}
			if response.Header == nil || response.Header.Revision == 0 {
				t.Error("Status carried no revision header")
			}
			// db_size must be a real measurement, not a constant zero: a zero
			// would understate the datastore in the apiserver's storage metric
			// forever.
			if response.DbSize == 0 {
				t.Error("Status reports db_size 0 after a write")
			}
		})
	}
}

// parseSemver accepts the dotted numeric form the apiserver's version.ParseSemantic
// expects, without pulling in that dependency for one assertion.
func parseSemver(value string) ([3]int, error) {
	var parts [3]int
	rest := value
	for i := 0; i < 3; i++ {
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == 0 {
			return parts, errNotSemver
		}
		n := 0
		for _, c := range rest[:end] {
			n = n*10 + int(c-'0')
		}
		parts[i] = n
		if i == 2 {
			return parts, nil
		}
		if end >= len(rest) || rest[end] != '.' {
			return parts, errNotSemver
		}
		rest = rest[end+1:]
	}
	return parts, nil
}

var errNotSemver = errors.New("not a dotted numeric version")
