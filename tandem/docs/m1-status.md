# Tandem M1 status

Issue #616 defines M1 as a real etcd v3 compatibility layer, not a protocol
type catalogue. The implementation must satisfy all of these before M1 can be
called complete:

| Area | Current status | Required next step |
| --- | --- | --- |
| gRPC/TLS transport | Linux TLS/mTLS Watch integration validated | Keep official-client interoperability in CI; document io_uring runtime requirements |
| KV/MVCC | production persistence connected | Startup requires rqlite and restores revision/compaction; KV and history survive restart. Complete concurrent/differential semantics validation |
| Txn | transaction-local Range answers from the committing transaction | Validate the remaining compare targets/options and the previous-delete response shape against etcd. Nested transactions are outside the promised surface (Appendix A scopes Txn to put/delete/get/range) and are refused with `Unimplemented`, which is the required handling for a call the layer does not serve |
| Watch | cross-process fanout validated on Linux | Live transaction events, historical replay, filters, cancellation, compaction errors and a watcher on one process observing a peer's commit are covered; still to validate slow consumers and recovery |
| Lease | background reaper with a single expiry owner, heartbeat-takeover after owner loss | Grant, keepalive, expiry, lease-driven deletes, owner exclusivity and takeover of a crashed owner's leases are covered against a real rqlite; qualify under real multi-node clock skew and partition |
| Compaction | watermark and historical-read boundary enforced | Verify interaction with concurrent readers and the SQL history/space reclamation path |
| Maintenance | Status reports real db size and a parseable version; Snapshot returns an explicit `Unimplemented` | Alarm/Defragment/Hash remain constant-valued; decide each mapping or return an explicit error |
| Single/three member | single node only | Share one semantic engine and add rqlite cluster lifecycle management |
| Differential tests | harness landed; KV, revision, tombstone, watch-order and error-code cases run against a real etcd, and it found five real divergences (see below) | Still to cover: compaction with concurrent readers, lease expiry under leader change, RangeStream under its feature gate, crash recovery |
| Recovery/concurrency | single-node restart, CAS and forced rqlite restart covered | Extend to multi-node crash recovery, concurrent histories and differential checks |

## Defects found and fixed in the M1 audit

These were found by running the compatibility layer against a real rqlited
rather than the in-memory store, which is why the in-memory lease tests had
been green throughout.

* **Persistent lease expiry deleted nothing.** Four independent defects, each
  sufficient on its own: the reaper read the BLOB `lease_keys.key` through
  `.text` and got an empty slice; the lease statements embedded the key in a
  single-quoted TEXT literal, so a binary key was a syntax error that rolled
  back the whole batch; the guard named `lease` where the table column is
  `lease_id`; and the revision advanced *after* the delete under an `EXISTS`
  guard that could never match, so the tombstone was stored at a revision the
  store never recorded.
* **Lease deadlines mixed two clocks.** rqlite rewrites `now` in a write
  statement to a Raft-assigned instant so every replica applies the same write.
  On a UTC+8 node that instant reads eight hours ahead of the host clock, so
  comparing it against a read-path `strftime` meant no lease ever looked
  expired. The timestamp now travels with the statement, which also satisfies
  KIP-29 §6's requirement that a write not be computed independently per
  replica.
* **A Txn Range was answered by a second query after the commit.** KIP-29 §6
  forbids assembling the response outside the atomic operation, and the answer
  could belong to a later commit. The rows are now captured by the same
  rqlite request that applies the transaction.
* **`Maintenance.Snapshot` returned an empty body**, which decodes as a valid
  zero-length snapshot — the "empty success" the epic forbids. It now returns
  `Unimplemented` on the wire.

A second audit, run against a multi-process deployment rather than a single one,
found seven more defects. Each was invisible in-memory and none had a failing
test, because the in-memory store implements the same logic correctly:

* **A Txn's failure branch misfiled its Range results.** The capture assigned
  every Range op a global id (`success.len + i + 1`), but the parser indexed
  the response array by that number as if it were a branch-local index. When
  the success branch was non-empty and its compare failed, the failure branch's
  Range was either dropped or attached to another op's slot — silently wrong
  data. The parser now maps the global id back to a branch-local index.
* **A Range inside a Txn ignored its own `limit`** and always reported
  `more = false`. The unary Range path did truncate and set `more`; only the
  Txn path lost it, so a controller paginating through a Txn would loop forever
  on a page that never ended.
* **A lease owner that crashed could never be replaced.** `owner` was cleared
  only on a graceful exit, so after `kill -9` the dead node's identity sat on
  the lease forever and no other instance could claim it. Ownership is now a
  heartbeat lease with a deadline, and every key deletion rechecks ownership in
  the same Raft transaction as the delete, so an instance that lost its claim
  while waiting cannot delete anything.
* **Lease ids were a per-process counter starting at 1000**, never persisted
  and never allocated through rqlite, so two instances granting at the same
  time could hand out the same id. A shared `lease_sequence` row now allocates
  them, and an explicit grant advances the sequence too.
* **The response header carried a possibly stale in-process revision.** With
  one rqlite behind several Tandem processes, a peer's commit did not refresh
  the reader's cache, so a read could answer with a header revision older than
  the data it returned. Every request now refreshes the shared revision and
  compaction watermark before it is served.
* **Watch events never crossed process boundaries** — only watchers on the
  process that applied a write were notified, which the documentation already
  conceded was unfinished. Watchers now follow one revision cursor over shared
  history, so a watcher on one instance sees a peer's commit.
* **`Put`/`DeleteRange` read `prev_kv` and the deleted count before the write**,
  as two independent requests, so another write could land in between and the
  reported previous values were not the ones actually overwritten. Both now run
  inside a single request: the delete captures the rows it removes into a
  temporary table before mutating them. The Txn path was already atomic, so the
  single-write path was the one that was not.

A third pass ran the operations against a **real embedded etcd** rather than
against Tandem's own expectations, which is the only way to catch a rule that
is wrong in the same way on both sides. It found three more:

* **`LeaseTimeToLive` reported an unknown or expired lease as `NotFound`.**
  etcd answers with a successful response carrying TTL `-1`, and the
  apiserver's lease manager reads that `-1` to decide a lease is gone. A
  `NotFound` was indistinguishable from a transport failure, so the caller
  retried a lease that would never come back. An expired lease also reported
  TTL `0` where etcd reports `-1`. Both now report `-1` with a zero granted TTL.
* **A read at a revision the store had not reached returned an empty success.**
  Only the compaction watermark was checked, so a future revision simply
  matched no history row and answered exactly as a read of an absent key
  would — silently wrong, and the caller could not tell a stale read from a
  missing object. It now returns `ErrFutureRev`'s `OutOfRange`, as etcd does.
* **Every unrouted method reported `INTERNAL` instead of `UNIMPLEMENTED`.**
  The apiserver reads `unimplemented` as "this endpoint lacks the feature" and
  falls back; `internal` reads as a temporary fault and is retried. The two
  are opposites, and a fallback that never triggers is how a missing RPC turns
  into an outage.

The suite also had to learn that etcd's Go client does **not** surface these
failures as gRPC statuses — the interceptors return `rpctypes.EtcdError`, which
carries a `Code()` but no `GRPCStatus()`. Reading the code through
`status.FromError` reported `Unknown` for every failure, which would have made
the comparison vacuous. The normalizer handles that type explicitly.

Two more surfaced once the suite covered compaction and historical reads, and
both were verified against a real etcd before being changed:

* **A read at the compaction revision itself was refused.** The guard was
  `revision <= compact_revision`, but compaction drops revisions *strictly
  below* the watermark — the boundary revision is retained, and etcd's own
  guard is `rev < compactMainRev` (`kvstore_txn.go`). A controller resuming
  from exactly its last compacted revision got a spurious `OutOfRange` for a
  read etcd serves. The same off-by-one was in the watch-resume guard, where
  etcd compares `minRev < compactionRev` (`watchable_store.go`).
* **A Range inside a Txn ignored its own revision and answered from the live
  table.** The op was reduced to key and range_end, dropping `revision`, so a
  read at an older revision returned the *current* value. That is wrong data
  with nothing in the response to distinguish it from a correct read: asking
  for revision 5 after the key had moved to revision 6 returned the revision-6
  value. The op now carries its revision, the capture reads `kv_history` when
  one is named, and an impossible revision is refused before the transaction
  runs, as etcd does in `etcdserver/txn/range.go`.

## Running the Linux integration suite locally

The tagged suite needs a Linux Tandem binary, so on macOS it has to run
somewhere Linux. Two constraints, both properties of the build rather than of
the code under test:

* `zig build native` is Linux-gated and the vendored grpc-lite needs
  cmake/ninja, so build it in a container. Delete `tandem/.zig-cache` first if
  it was populated on the host, or the C objects are the wrong architecture.
* Tandem's libxev transport uses `io_uring`, which Docker's default seccomp
  profile denies, so the container needs `--security-opt seccomp=unconfined`.
  This mirrors what `hack/e2e/lib.sh` already does for the E2E profiles; it is
  a test-harness setting and not a production recommendation.

```sh
docker run --rm --platform linux/arm64 --security-opt seccomp=unconfined \
  -v "$PWD":/w -v /path/to/linux-bin:/b:ro \
  -v "$HOME/go/pkg/mod":/go/pkg/mod -v "$HOME/.cache/go-build":/root/.cache/go-build \
  -w /w golang:1.27-alpine sh -c '
    apk add --no-cache rqlite >/dev/null 2>&1
    TANDEM_TEST_BINARY=/b/tandem \
    TANDEM_TEST_PROBE=/b/persistence-probe \
    TANDEM_TEST_RQLITED=/usr/bin/rqlited \
      go test -mod=mod -tags=tandem_integration ./pkg/tandem -count=1 -timeout=600s'
```

The container needs a musl rqlited (Alpine's package) to match the musl Tandem
binary; a glibc rqlited from the rqlite release will not exec in the same
container. On the same run all 34 tests pass.


The process must not report readiness while the transport is unavailable. The
entry point therefore propagates `grpc-lite.Server.start()` and TLS
configuration errors instead of logging a misleading ready message.

Production uses `PipelineServer.initPersistent`; `initMemory` is only for
explicit in-memory tests. rqlite HTTP, SQL and initial-state errors propagate
to startup, never falling back to volatile storage. Writes use
`/db/execute?transaction`; queries use strong consistency. Txn uses
`/db/request?transaction` so its branch decision, writes, revision and operation
results share one Raft transaction. The temporary result table is consumed and
dropped within that request. Reading it later through `/db/query` is invalid:
that connection cannot see the TEMP table, and a shared persistent scratch table
would allow another process to overwrite the results.

The PR #622 startup failures had two causes: Docker's default seccomp profile
blocked libxev's `io_uring` initialization in L1, and persistent Txn emitted
malformed SQL before querying its result table from another connection in L2.
The E2E harness now explicitly permits io_uring with `seccomp=unconfined` for
these disposable test containers. This is a test-harness setting, not a change
to sandbox runtime isolation or a production deployment recommendation.

Validated locally with OrbStack Linux arm64 and rqlite v10.3.6:

- L1: 30 checks, including control-plane restart and persisted namespace recovery.
- L2: 24 checks, including kubelet/containerd/runc, hostNetwork Pod execution,
  logs and restart recovery.
- Official-client Txn regression: create-if-absent, binary values, previous KV,
  one revision for multiple writes, read-only failure branch, exact-key and
  empty deletes, concurrent CAS, and a Range inside a Txn honouring its own
  `limit` — truncated page, full match count and `more` set.
- Independent Pipeline processes contend on one rqlite using CAS; exactly one
  of eight wins. This does not qualify three-node lifecycle or failover.
- A watcher on one Tandem process receives a commit made by a peer against the
  same rqlite, and a lease whose owner was left behind by a crash is taken over
  by a live instance. This does not qualify three-node lifecycle or failover.
- `go test -tags=tandem_integration ./pkg/tandem` on native Linux arm64 against
  a real rqlited: all 34 tests pass, including the official-client TLS/mTLS
  Watch test, the persistent transaction test, lease expiry, owner exclusivity,
  crash takeover, shared lease-id allocation, standalone previous values and
  cross-process watch fan-out.
- `zig build test`: 155/155.

The L1 CI job also runs the tagged Tandem integration suite against its pinned
rqlited and built Tandem binary, so persistence and TLS regressions no longer
depend solely on manual runs. CI must pass on the pushed head before claiming
Linux amd64 qualification.

Remaining merge gates: complete compare/options semantics and the
previous-delete response shape; `KV.RangeStream` and the Maintenance/Cluster/Auth
services are still unregistered, so the "etcd v3.7 compatible" claim does not
hold for them; the driver's Snapshot/Reset/Restore are still unimplemented, so
`k8e etcd-snapshot` style operational workflows are unavailable on this
backend; multi-node join/readiness/failover; removal of embedded-etcd
production paths; reset/snapshot/restore; the full etcd differential suite,
the Go full suite and Kubernetes E2E on a normal Linux environment. M1
remains open.

Portable persistence regression (requires Go, Zig and a native rqlited):

```sh
cd tandem
zig build persistence-probe
cd ..
TANDEM_TEST_RQLITED=/absolute/path/to/rqlited \
TANDEM_TEST_PROBE="$PWD/tandem/zig-out/bin/persistence-probe" \
go test -tags=tandem_integration ./pkg/tandem -run TestTandemPersistenceRestart -count=1
```

The fixture starts an isolated rqlite, writes through the production Pipeline
initializer, reads in another process, kills/restarts rqlite, and verifies KV,
revision and Watch history again. The Linux Watch/TLS test also requires
`TANDEM_TEST_RQLITED`; it no longer relies on an ambient localhost daemon.
This probe is not a substitute for Linux gRPC/TLS or full Kubernetes E2E tests.
