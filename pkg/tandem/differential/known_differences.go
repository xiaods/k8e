// Package differential compares the Tandem compatibility layer against a real
// embedded etcd, operation by operation.
//
// The unit and integration suites assert Tandem against its own expectations.
// That cannot catch a semantic divergence, because both the in-memory store
// and the rqlite store implement the same intent — a rule that is wrong in the
// same way on both sides passes everywhere. Only running the identical
// operation against a real etcd and comparing the answers exposes that class
// of defect, which is what KIP-29 §10 makes the M1 exit criterion.
//
// The baseline is the k3s fork of etcd 3.7.1, because that is the etcd K8E
// actually ships and therefore the semantics Tandem has to match.
package differential

// Difference records a place where Tandem is expected to answer differently
// from etcd. Every entry needs a Reason and a Regression trigger, otherwise
// the list rots into a blanket suppression and stops describing anything.
type Difference struct {
	// Method is the etcd RPC, e.g. "Maintenance.Snapshot".
	Method string
	// Observed describes what Tandem does today.
	Observed string
	// Reason explains why the difference is acceptable.
	Reason string
	// Regression states what would make this entry wrong — a change that
	// should cause the entry to be deleted and the difference treated as a
	// bug rather than a known gap.
	Regression string
}

// KnownDifferences is the registry of accepted divergences. It is deliberately
// explicit: an unlisted difference fails the test, so a new divergence is
// caught rather than absorbed.
var KnownDifferences = []Difference{
	{
		Method:     "Maintenance.Snapshot",
		Observed:   "Unimplemented.",
		Reason:     "rqlite snapshots are SQLite files, not etcd snapshot files, so there is nothing to return. KIP-29 class B maps the K8E flow onto rqlite /db/backup.",
		Regression: "Answering with a real body. An empty body decodes as a valid zero-length snapshot, which is the empty success KIP-29 forbids.",
	},
	{
		Method:     "Maintenance.MoveLeader",
		Observed:   "Not routed, so Unimplemented. The handler exists but returns an empty header as if it had moved a leader, so registering it would be a false success.",
		Reason:     "Leadership belongs to rqlite's Raft. The compatibility layer has no leader to move and must not imply it did.",
		Regression: "rqlite gaining a leader-transfer API the layer can honestly report.",
	},
	{
		Method:     "Maintenance.Alarm",
		Observed:   "Not routed, so Unimplemented. The handler decodes the request and ignores it.",
		Reason:     "rqlite has no disk-alarm model. A GET on a healthy etcd also returns empty, so the shapes happen to agree, but issuing ACTIVATE against etcd would arm a real NOSPACE alarm and permanently diverge the two stores.",
		Regression: "rqlite exposing a real alarm or disk-health signal the layer could map.",
	},
	{
		Method:     "Maintenance.Defragment",
		Observed:   "Not routed, so Unimplemented.",
		Reason:     "Defragmenting a bbolt file has no SQLite equivalent; rqlite vacuums on its own schedule.",
		Regression: "rqlite exposing a vacuum trigger the layer can surface honestly.",
	},
	{
		Method:     "Maintenance.Hash and Maintenance.HashKV",
		Observed:   "Not routed, so Unimplemented. The handlers return hash 0.",
		Reason:     "A content hash over bbolt has no meaning against SQLite. Returning 0 would be a plausible-looking number that is always wrong.",
		Regression: "A defined cross-store hash, which rqlite does not offer.",
	},
	{
		Method:     "Cluster.* and Auth.*",
		Observed:   "Not routed, so Unimplemented. The handlers return empty member lists or constant replies.",
		Reason:     "KIP-29 class B: membership is rqlite's own, and K8E authenticates with mTLS rather than etcd's auth service. etcd member IDs and learner promotion are explicitly not emulated.",
		Regression: "A member list that reflects rqlite's real membership rather than an empty constant.",
	},
	{
		Method:     "Maintenance.Status",
		Observed:   "Not routed, so Unimplemented. The handler fabricates leader 1 and raft indices of 1; only db_size is real, and it measures the SQLite file.",
		Reason:     "Most of the reply would be constant regardless of cluster state. An empty or constant success is what KIP-29 forbids, so the honest answer is that it is not served yet.",
		Regression: "Reporting rqlite's real /status values, at which point it becomes comparable and the difference is deleted.",
	},
	{
		Method:     "KV.RangeStream",
		Observed:   "Not implemented; no handler and no route.",
		Reason:     "The apiserver's EtcdRangeStream gate is Beta and on by default in 1.37, so this is a real gap rather than a cosmetic one, and the K8E call surface is not fully covered until it lands.",
		Regression: "Nothing. If this ever stops differing it is because RangeStream shipped, which is an improvement to record rather than suppress.",
	},
	{
		Method:     "KV.Txn with a nested Txn",
		Observed:   "Unimplemented.",
		Reason:     "Appendix A scopes Txn to put/delete/get/range, and nesting is outside the promised surface.",
		Regression: "Nothing; nested transactions stay out of scope.",
	},
}

// Lookup returns the registry entry for a method, if one exists.
func Lookup(method string) (Difference, bool) {
	for _, difference := range KnownDifferences {
		if difference.Method == method {
			return difference, true
		}
	}
	return Difference{}, false
}
