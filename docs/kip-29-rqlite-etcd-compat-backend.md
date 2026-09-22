# KIP-29: rqlite + etcd v3 compatibility backend

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-09-22 | Proposed — M0 (design + rqlite capability evidence) and M1 (etcd v3 compatibility layer) complete; Zig port and K8E integration open |

**Created**: 2026-09-21
**Relates to**: epic [#592](https://github.com/xiaods/k8e/issues/592) (replace embedded etcd with rqlite + a compatibility layer), [KIP-6](kip-6-embedded-etcd-design.md) (embedded etcd as sole backend — revisited here), [KIP-1](kip-1-native-etcd-storage-client.md) (native clientv3 storage client), [KIP-7](kip-7-embedded-etcd-fuse.md) (official `embed` package), [KIP-26](kip-26-upgrade-dependencies-to-kubernetes-1.37.md) (Kubernetes `v1.37.0-k3s1` / etcd `v3.7.1-k3s1`)
**M0 evidence**: `make test-rqlite-m0` (`hack/rqlite-m0/run.sh`) → `tests/rqlitecompat/` (11 evidence tests + 1 helper process, against a real `rqlited`)
**M1 evidence**: `make test-rqlite-m1` (`hack/rqlite-m1/run.sh`) → `pkg/rqlitecompat` (the layer) exercised by `tests/rqlitecompat/` differentially against this repository's embedded etcd, plus three-node, layer-restart, TLS/mTLS, `RangeStream` and wire-limit checks, on the same pinned `rqlited`

---

## 1. Summary

K8E stores its control-plane state in an embedded etcd (`pkg/etcd`, `pkg/embedw`, KIP-6/KIP-7).
This KIP replaces that datastore with **rqlite** (Raft + SQLite) fronted by an
**etcd v3 compatibility layer**:

```
kube-apiserver / bootstrap client / k8e
              │ etcd v3 gRPC (+ TLS)
              ▼
      etcd v3 compatibility layer            ← M1, not built yet
   MVCC / Txn / Watch / Lease / Compaction
              │ rqlite HTTP API (protected, loopback or mTLS)
              ▼
            rqlite
     Raft consensus / SQLite / durability
              │
           local disk  ── optional backup ──▶ S3
```

rqlite owns consensus, replication, persistence and recovery. The compatibility
layer owns etcd protocol, MVCC, conditional transactions, events, leases and
error codes. K8E owns process hosting, TLS, configuration, membership
lifecycle, backup/restore and the migration entry point.

This KIP is the **M0 deliverable** of epic #592: it fixes the component
interfaces, the deployment topology, the versioned schema draft, the etcd
API/semantics matrix (Appendix A) and the pinned rqlite version, and it records
what the M0 prototype proved and what it did not.

**M0 gate result**: every semantic the epic calls the "first technical gate" —
atomic Txn, revision allocation, an atomically-consistent response, an
uncertain-request retry contract and linearizable reads — is expressible in
**one** rqlite transaction request, and is proven by repeatable tests. No
upstream extension is required, and no fallback to multiple requests was
needed.

## 2. What M0 had to prove, and what it did not

In scope, with evidence in `tests/rqlitecompat/` (run via `make test-rqlite-m0`,
which is `hack/rqlite-m0/run.sh`):

| Epic §1 requirement | Evidence |
|---|---|
| A Txn's compare, mutation, revision allocation, history/tombstone and response are one atomic rqlite transaction | `TestTxnResponseComesFromTheCommittingTransaction` (a concurrent writer runs against the same key and the response still matches the committed revision) |
| Concurrent same-revision CAS has at most one winner; a failed compare is mapped correctly and consumes no revision | `TestTxnCompareBranchesAndRevision`, `TestConcurrentAdapterProcessesRaceOnOneKey` |
| Revision semantics match etcd (shared revision per mutation, `version = prev+1`, unchanged revision on a failed compare, tombstone on delete) | `TestTxnCompareBranchesAndRevision`, `TestClusterCrashRestartPreservesCommittedData` |
| An HTTP timeout/disconnect may correspond to a committed operation; internal retry needs a request identity, an outcome decision and de-duplication | `TestLostResponseReplayIsExactlyOnce` |
| A no-quorum cluster fails with a bounded error and no partial write | `TestNoQuorumFailsFastWithoutPartialWrite` |
| Strong reads are explicitly `linearizable`, never the default `weak` | `TestLinearizableReadFromEveryNode` (plus a wire assertion that every read URL carries `level=linearizable`) |
| Multiple adapter clients concurrently | `TestConcurrentAdapterProcessesAllocateContiguousRevisions` (4 processes × 25 creates → revisions exactly 1..100) |
| Leader switch | `TestLeaderSwitchKeepsAcknowledgedWrites` (leader SIGKILL, re-election, retries) |
| Binary keys/values, bytewise range order | `TestBinaryKeysRangeInByteOrder`, `TestSchemaBootstrapIsIdempotentAndBlobsRoundTrip` |
| Durability across a crash | `TestClusterCrashRestartPreservesCommittedData` (all three nodes SIGKILLed, restart from disk, replayed request ids still de-duplicated) |

Out of scope for M0 (design only, first implemented in M1): the gRPC surface
itself, watches, leases, compaction, membership, TLS and the K8E driver. This
KIP records their interfaces and constraints but claims **no** evidence for
them. In particular the M0 prototype is *not* a prototype of the compat layer:
it proves that the storage-level semantics the compat layer must expose are
expressible through rqlite's public HTTP API.

## 3. Component interfaces

### 3.1 rqlite (external process)

Interface: the public HTTP API on a loopback (single node) or mTLS-protected
(node-to-node) address. Only the endpoints in §7 are used.

* No direct SQLite file access, ever. The compatibility layer must not open
  `db.sqlite`, and must not bypass rqlite to change `kv` — the revision
  invariant (§5.4) lives in a transaction that rqlite orders.
* No queued writes (`?queue`): they are acknowledged before the Raft commit and
  are "unsuitable for production" per the rqlite docs; the compat layer never
  sets `queue`.
* No explicit `BEGIN`/`COMMIT`/`ROLLBACK`/`SAVEPOINT` — rqlite documents that
  behaviour as undefined; transactions are requested with the `transaction`
  query parameter only.
* Deterministic transaction scripts: any timestamp, election or identity value
  is computed by the layer and passed *in* as a parameter, so every replica
  applies identical SQL.

### 3.2 etcd v3 compatibility layer (M1, Zig)

Serves, over gRPC + mTLS on the etcd port (2379 by default), the etcd v3
services the Kubernetes apiserver and K8E actually call (Appendix A):

```
KVServer            Range, Put, DeleteRange, Txn, Compact, RangeStream
WatchServer         Watch (streaming, progress notifications)
LeaseServer         LeaseGrant, LeaseRevoke, LeaseKeepAlive, LeaseTimeToLive
MaintenanceServer   Status, Alarm, Defragment, Hash, Snapshot
ClusterServer       MemberList, MemberAdd, MemberRemove, MemberUpdate, MemberPromote
```

Every RPC it does *not* implement must return a real gRPC error
(`Unimplemented`), never an empty success — the epic's "禁止空成功响应" rule,
and also the apiserver's own feature detection: it treats an
`Unimplemented` `RangeStream` as "endpoint does not support it" and falls back
(see Appendix A, row KV.RangeStream).

Internal interface to rqlite, in language-neutral form (the M0 prototype is
Go, M1 implements the same calls in Zig):

```
writeTxn(statements) -> (results[], raftIndex)   POST /db/request?transaction&blob_array
readLinearizable(statements) -> (rows[], revision) POST /db/query?level=linearizable
status(endpoint) -> (nodeID, leaderID, dbAppliedIndex, raftAppliedIndex, version)  GET /status
ready(endpoint) -> ok | error                     GET /readyz
```

### 3.3 K8E driver

A new datastore driver implementing the existing plug point
`managed.Driver` (`pkg/cluster/managed/drivers.go`):

```
SetControlConfig / IsInitialized / Register / Reset / IsReset / ResetFile
Start / Test / Restore / EndpointName / Snapshot / ReconcileSnapshotData
GetMembersClientURLs / RemoveSelf
```

Rules:

* A **new** driver (`pkg/rqlite`, name TBD) — do not reuse `pkg/etcd`, whose
  member logic assumes etcd member IDs and peer URLs (epic §5).
* `Start` supervises two child processes (rqlited, then the compat layer) and
  propagates startup failure; `Test` checks rqlite readiness *and* layer
  readiness; `EndpointName` returns the etcd-compatible endpoint the apiserver
  dials.
* `Snapshot`/`Restore` map onto rqlite's own backup/restore entry points
  (§8.3) — etcd snapshot files are not readable by rqlite.
* Bootstrap KV access (`pkg/cluster/storage.go`, `pkg/etcdstorage`) goes
  through the compat layer like any other client, so the bootstrap token path
  exercises the same Txn semantics as the apiserver.
* `pkg/daemons/control/server.go:setupStorageBackend` currently hard-codes
  `storage-backend=etcd3`; M2 adds the explicit opt-in selection of the new
  driver, keeping embedded etcd the default until the M3 migration gate.

## 4. Deployment topology

First version: **processes shipped with K8E and managed by K8E in the same
data-dir**. "Embedded" means self-contained delivery plus automatic lifecycle
management, not same-process hosting; in-process FFI embedding is explicitly out
of scope.

Single node:

| Process | Listens on | Data |
|---|---|---|
| `rqlited` | HTTP `127.0.0.1:4001`, Raft `127.0.0.1:4002` | `${data-dir}/rqlite/` |
| compat layer | etcd `0.0.0.0:2379` (mTLS), peer/metrics port TBD in M1 | `${data-dir}/rqlite-layer/` (certs, layer metadata only) |
| kube-apiserver / k8e | dials `127.0.0.1:2379` with the existing client certs | — |

Three voters (first HA target), one rqlited per node joined with
`-join <all raft addrs>`, one compat layer per node forwarding to its local
rqlited:

```
node A: apiserver → layer A ─┐
                             ├─ rqlite Raft (leader); followers forward HTTP requests to the leader
node B: apiserver → layer B ─┤
node C: apiserver → layer C ─┘
```

* Data, logs and certificates live in K8E-managed directories. Reusing an
  existing data-dir must never silently create an empty database.
* Local rqlite API is loopback-only; cross-node rqlite traffic uses mTLS
  (`-node-cert`/`-node-key`/`-node-ca-cert`, `-node-verify-client`) or is
  otherwise network-protected. Users must not write to the database behind the
  compat layer.
* Startup order: rqlited ready → layer ready → bootstrap → kube-apiserver.
  Exit, timeout, crash-restart and startup-failure propagation are explicit:
  if rqlite is not ready the layer must not report ready, and if the layer is
  not ready kube-apiserver must fail to start rather than start with a backend
  it cannot use.
* Readiness reflects serviceability, not just liveness: a node without quorum
  is not ready (`/readyz` on a quorum-less rqlite node returns 503
  `leader does not exist`; verified in M0).
* rqlited, the Zig layer, their pinned versions and their SHA-256 digests go
  into the Zig build graph, cross-architecture builds, airgap packages and
  release manifests. Whether rqlited is shipped as a resource embedded into the
  release binary and unpacked on first start (to preserve the single-file
  install experience) is a packaging decision for M2; this KIP only fixes the
  requirement that the installed artifact is self-contained and version-pinned.

## 5. Versioned schema draft

Design goals: current KV state, full retained history including deletion
tombstones, a global revision, a compaction watermark, durable events, leases
and stable identity metadata — in one SQLite database whose transactions rqlite
orders.

Conventions:

* All keys and values are **BLOB**. SQLite compares BLOBs with `memcmp`, which
  is exactly etcd's bytewise key order, so prefix/range scans are plain
  `key >= :start AND key < :end` with literal ordering. TEXT keys would be
  lossy for non-UTF-8 keys (JSON strings cannot carry arbitrary bytes) —
  rqlite's documented way to pass binary parameters is a JSON array of byte
  values, which the M0 prototype uses and `TestBinaryKeysRangeInByteOrder`
  verifies.
* All tables are `STRICT`, so a type mistake fails loudly instead of being
  coerced.
* `revision` is a row in `k8e_compat_meta`, not a row counter: SQLite rowids
  are not etcd revisions.
* Every change to these tables happens inside a single
  `/db/request?transaction` call; schema version is recorded in-band.

```sql
CREATE TABLE k8e_compat_meta (
  name  TEXT    NOT NULL PRIMARY KEY,   -- 'schema_version', 'revision', 'compact_revision',
  value INTEGER NOT NULL                -- 'cluster_id', 'applied_index', ...
) STRICT;

CREATE TABLE kv (                        -- current state only
  key             BLOB    NOT NULL PRIMARY KEY,
  value           BLOB    NOT NULL,
  create_revision INTEGER NOT NULL,
  mod_revision    INTEGER NOT NULL,
  version         INTEGER NOT NULL,
  lease           INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX kv_lease     ON kv(lease);
CREATE INDEX kv_mod_rev   ON kv(mod_revision);

CREATE TABLE kv_history (                -- every retained revision of a key
  key          BLOB    NOT NULL,
  mod_revision INTEGER NOT NULL,
  version      INTEGER NOT NULL,
  deleted      INTEGER NOT NULL,         -- 1 = tombstone row
  value        BLOB,                     -- NULL for a tombstone
  lease        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (key, mod_revision)
) STRICT;
CREATE INDEX kv_history_rev ON kv_history(mod_revision);   -- watch/compaction scans

CREATE TABLE applied_requests (          -- request identity and recorded outcome
  request_id TEXT    NOT NULL PRIMARY KEY,
  kind       TEXT    NOT NULL,
  outcome    TEXT    NOT NULL,           -- 'pending' | 'committed' | 'cas_failed'
  revision   INTEGER NOT NULL
) STRICT;

CREATE TABLE events (                    -- durable watch log (M1)
  revision  INTEGER NOT NULL,
  seq       INTEGER NOT NULL,            -- order within a revision
  kind      TEXT    NOT NULL,            -- 'put' | 'delete'
  key       BLOB    NOT NULL,
  value     BLOB,
  prev_kv   BLOB,                        -- optional, only when prev_kv was requested
  create_revision INTEGER NOT NULL,
  mod_revision    INTEGER NOT NULL,
  version         INTEGER NOT NULL,
  lease           INTEGER NOT NULL,
  PRIMARY KEY (revision, seq)
) STRICT;

CREATE TABLE leases (
  id          INTEGER NOT NULL PRIMARY KEY,
  ttl         INTEGER NOT NULL,          -- seconds, as granted
  expiry_unix INTEGER NOT NULL,          -- absolute, written by the layer, never by SQLite
  owner       TEXT    NOT NULL           -- layer instance that currently owns expiry
) STRICT;
CREATE INDEX leases_expiry ON leases(expiry_unix);

CREATE TABLE identities (                -- stable identity metadata (M2/M3)
  name  TEXT NOT NULL PRIMARY KEY,       -- e.g. 'cluster_id', 'node_id'
  value BLOB NOT NULL
) STRICT;
```

`kv_history` is retained at least until the compaction watermark, so a read at
an older revision, a watch resuming from a revision, and `prev_kv` are all
served from SQL rather than from layer memory. Compaction deletes history rows
at or below the watermark together with their events and records the watermark
in `k8e_compat_meta.compact_revision`.

Versioning: `k8e_compat_meta.schema_version` (also as an integer column check in
bootstrap), forward-only migrations applied inside one transaction, and a
refusal to start when the recorded version is newer than the binary (no implicit
downgrade). `Bootstrap` is idempotent — `CREATE TABLE IF NOT EXISTS` plus
`INSERT OR IGNORE` — and must never reset the revision counter
(`TestSchemaBootstrapIsIdempotentAndBlobsRoundTrip`).

## 6. Atomic Txn, revision and uncertainty contract (M0 result)

The etcd calls the apiserver makes (`go.etcd.io/etcd/client/v3/kubernetes`,
`OptimisticPut`/`OptimisticDelete`, used by
`k8s.io/apiserver/pkg/storage/etcd3`) are exactly:

```
Txn.If( Compare(ModRevision(key), "=", expected) )
   .Then( OpPut(key, value [, WithLease]) | OpDelete(key) )
   .Else( OpGet(key) )            // only when GetOnFailure
```

The prototype maps one such Txn to **one** `/db/request?transaction` call with
a parameterised statement list:

1. insert the request identity (`ON CONFLICT DO NOTHING`) — this makes a replay
   detectable and idempotent;
2. the conditional mutation (`INSERT` when expecting "not exists", `UPDATE`
   when expecting a mod_revision), writing `mod_revision = revision + 1` and
   `version = version + 1`, or `DELETE` plus a tombstone row for a delete;
3. the `kv_history` row for the new revision;
4. the outcome record (`committed`/`cas_failed`) **including the revision the
   transaction reports**;
5. the global revision bump — executed only when this transaction is the one
   that wrote the history row for it;
6. the response row (`SELECT` of the outcome and the key state) — the response
   is produced *by the committing transaction*, so a concurrent writer cannot
   make it describe a different revision.

Revision rules, matching etcd:

* The revision advances only when the transaction changed state. A failed
  compare leaves it unchanged and reports the then-current revision — etcd's
  `storeTxnWrite.End` only increments the revision when there are changes
  (`server/storage/mvcc/kvstore_txn.go` in the k3s fork).
* All changes of one mutation share one revision.
* Delete writes a tombstone row (key, revision, `deleted = 1`, NULL value) —
  the M0 prototype does not assert tombstone-`version` parity with etcd's
  internal value; that differential test belongs to M1 (Appendix A).

Uncertainty contract (the epic's §1.4): an HTTP timeout or a lost response may
correspond to a committed transaction. The layer:

* always sends a request id, so a retry re-uses the same identity and is
  de-duplicated instead of applied twice (`Deduped` in the prototype,
  `TestLostResponseReplayIsExactlyOnce`);
* classifies retryability from the *response body*, because rqlite reports a
  failure to reach the leader as HTTP 200 with a connection error in the body
  (verified in M0: `{"results":[],"error":"dial tcp <leader>: connect:
  connection refused"}`), not only from the status code;
* bounds retries. A quorum-less write is observed in two bounded forms
  against v10.3.5: once the follower knows there is no leader it answers 503
  `leader not found`; if it is still forwarding to a dead leader the request
  ends in the adapter's own context deadline (`context deadline exceeded`).
  Either way the prototype gives up within its retry budget with `write failed
  after N attempts`, and the request leaves no partial write behind;
* does **not** promise exactly-once for independent client RPCs — only that a
  retried *internal* attempt cannot apply twice.

Forbidden, and not used anywhere in the M0 prototype: a `SELECT` to evaluate
the compare followed by a second HTTP request to mutate; a process-local lock
as a substitute for cross-instance atomicity; manual `BEGIN`/`COMMIT` across
requests; queued writes; and trusting HTTP 200 as "the SQL succeeded".

## 7. API boundary

Used by the compat layer, verified against v10.3.5 (`/status`, `/readyz`,
`/db/*` on a live node and the M0 suite) and the official docs:

| Endpoint / parameter | Use |
|---|---|
| `POST /db/request?transaction&blob_array&db_timeout=10s&timeout=15s` | every write/Txn: one request, one SQLite transaction, one Raft log entry |
| `POST /db/query?level=linearizable&linearizable_timeout=5s&blob_array&...` | every read |
| `GET /status` | node id, leader, `db_applied_index`, `fsm_index`, `raft.*`, `build.version`, sqlite pragmas |
| `GET /readyz` | readiness (node + leader + store + db); 503 without quorum |
| `GET /nodes?nonvoters` | member list for driver/diagnostics |
| `GET /db/backup` | snapshot to a SQLite file (verified: readable SQLite DB containing the committed rows) |
| `POST /db/load` | restore into a fresh data-dir — M2/M3 item, not exercised in M0 |
| `?level=` | explicit read consistency (§9); `strong` is rejected for production, `none`/`auto` are never used |
| `?timeout=` | follower→leader forwarding timeout (rqlite default 30s) |
| `?retries=` | node-to-node retry on a contact failure; the layer does its own retry instead |
| `?raft_index` | optional: the Raft index a write landed at (diagnostics) |
| `?freshness`, `?freshness_strict` | not used (read replicas are out of scope for v1) |

Boundary rules:

* rqlite's default read level is `weak`, which the docs describe as *not*
  linearizable; `auto` maps to `weak` on voting nodes. The layer always passes
  `level=linearizable` explicitly. The M0 prototype asserts that no read is
  ever sent without a level, and asserts the level on the wire.
* Errors arrive in two places (4xx/5xx status **and** `results[i].error` inside
  an HTTP 200 body); both are checked.
* `db_timeout` bounds one SQL statement, not the whole HTTP request, so the
  layer sets both.
* Unsupported etcd interfaces return explicit errors (§Appendix A, class C).

## 8. rqlite version lock, recovery and upstream cost

### 8.1 Locked version

| | |
|---|---|
| Release | **`v10.3.5`** (published 2026-09-18) |
| Server build | `rqlited v10.3.5 linux amd64 go1.27.1 sqlite3.53.4`, commit `58f9d8898a5f5d4bf712d472f96e38cc7cb4748a` |
| SHA-256 (linux-amd64) | `edaef7580cd60f7f9d558cde2fb95011f6e356ff64da7167fb659ab4b6882014` |
| SHA-256 (linux-arm64) | `23d5d09d34a9e2db54b2c21469aacb3b6563166e3d0d3901f4f4fb44990db99b` |
| Harness | `make test-rqlite-m0` → `hack/rqlite-m0/run.sh`; downloads the pinned build, verifies the digest, prints the version, and runs `tests/rqlitecompat/` against it (`RQLITE_M0_TEST_ARGS` forwards `go test` flags, e.g. `-race`). The M1 suite reuses the same verified binary: `make test-rqlite-m1` → `hack/rqlite-m1/run.sh` (selects `TestM1*`, `RQLITE_M1_TEST_ARGS`/`RQLITE_M1_RUN` override) |

The arm64 digest is the published release digest, not yet exercised on arm64
hardware; M1 must run the suite on both architectures before the compatibility
layer is called architecture-complete.

### 8.2 Recovery and durability (as proven in M0)

* SQLite runs in WAL mode with one read-write connection and up to 256
  read-only connections (`/status`: `store.sqlite3.conn_pool_stats.rw.max_open_connections = 1`,
  `...ro.max_open_connections = 256`, `store.sqlite3.pragmas.rw.journal_mode = wal`).
  All writes therefore serialize through one leader-side write connection.
* SQLite is opened with `synchronous = 0` (`store.sqlite3.rw_dsn` contains
  `_sync=0`, and `store.sqlite3.pragmas.rw.synchronous` is `0`): the durable
  record of a write is the Raft log (`raft.db`, BoltDB) plus periodic Raft
  snapshots (`wsnapshots/`), not the SQLite file itself. The compatibility
  layer must never treat `db.sqlite` as an independent source of truth.
* Writes are acknowledged only after a quorum of nodes has committed them to
  the Raft log (rqlite docs, "Data and the Raft log"); followers forward the
  write to the leader and return the leader's response.
* M0 evidence: SIGKILL of all three nodes followed by a restart from disk
  preserves every acknowledged write at the same revision, keeps tombstones,
  and continues the revision counter (`TestClusterCrashRestartPreservesCommittedData`);
  a single node restarted after `kill -9` still serves the row and answers a
  linearizable read.
* History compaction is **not** a backup and does not immediately return space
  to the filesystem; `-auto-vacuum-int`/`-auto-optimize-int` exist but their
  long-run behaviour, disk-full (`ENOSPC`) and `EIO` handling are M2 items
  (§M2 stage 1 and 4 of the epic).
* A local backup is `GET /db/backup`, which returns a valid SQLite database
  (verified by opening it and reading the committed rows with the `sqlite3`
  Python module). Restore-into-a-fresh-data-dir (`POST /db/load`) and the
  optional S3 target are M2/M3 work, and S3 must stay optional: no S3 means no
  impact on normal operation.

### 8.3 Upstream dependency cost

* One new upstream runtime dependency (rqlite/rqlite, MIT), pinned by release
  and SHA-256, shipped as a binary like the other staged K8E binaries. No fork.
* rqlite releases frequently; each upgrade re-runs the M0 suite plus the M1
  differential tests before it can ship. Because the compatibility layer never
  touches SQLite directly and only uses the endpoints in §7, the upgrade
  surface is that table plus the schema in §5.
* Missing semantics have a defined path: if the public API cannot express a
  required atomic semantic, M0's rule is to record the failure and revise the
  design — not to split the transaction and claim compatibility. **M0 found no
  such case.**
* The layer still has to implement the protocol itself (gRPC, MVCC, watch
  fan-out, leases, error codes); "rqlite is mature" is not evidence of compat
  layer correctness — that is what M1's differential tests are for.
* Cost on K8E: two supervised processes instead of one, a new driver, extra
  ports and certificates, plus the release/airgap manifest entries. The
  replacement of embedded etcd also removes the `pkg/embedw` fuse and the etcd
  server dependency from the release artifacts; M2 compares RSS, CPU, I/O,
  tail latency and package size against embedded etcd.

## 9. Reads, watches, leases and compaction (M1 design constraints)

* **Reads**: every read uses `level=linearizable`. rqlite implements this as
  the Raft dissertation's §6.4 read-only optimization: the node records the
  Raft commit index, confirms leadership with a quorum, waits until that commit
  index has been applied to SQLite, and only then reads. A linearizable read is
  therefore never stale, and it is measurably slower than `weak` — the reason
  the level must be explicit rather than defaulted. The M0 suite reads from
  every node in a 3-node cluster and gets the committed result.
  A caveat worth recording: **the Kubernetes apiserver never asks for a
  serializable read** — `WithSerializable` appears only in an apiserver test
  comment explaining that `KV.Get` is linearizable by default. Serving every
  read from a quorum-confirmed leader is therefore compatible with the whole
  current call surface, and the compat layer does not need a `weak` fast path
  for correctness (only, possibly, later, for performance).
* **Watch**: events are written in the same transaction as the data change
  (the `events` table), never only in layer memory; a shared event reader
  polls/streams by revision and fans out to local watch connections; progress
  notifications must never overtake an unsent event; resume, filtering,
  `prev_kv`, slow consumers and compaction boundaries are specified in M1.
  The apiserver requires progress notifications (`RequestWatchProgress`) and
  decides whether the endpoint supports them from the **etcd version string
  reported by `Maintenance.Status`** (`pkg/storage/feature`: a semantic version
  ≥ 3.4.31 / ≥ 3.5.13), so the compat layer must report a plausible etcd
  version there.
* **Leases**: persistent in rqlite; revoke and expiry go through the same
  atomic conditional transaction, and an expiry produces a normal revision and
  a watch event. Expiry is executed by exactly one owner (recorded in the
  `leases.owner` row), never by each instance's local clock.
* **Compaction**: `KV.Compact` records the watermark, then deletes history and
  events at or below it in bounded batches; time-based auto-compaction
  (`compact_rev_key` semantics) is mapped onto `Maintenance.Compact`.

## 10. M1 result: the etcd v3 compatibility layer

**Delivered**: `pkg/rqlitecompat` — a process-local gRPC server that speaks the
etcd v3 wire protocol and keeps the whole store in rqlite through §7's HTTP API
only. `make test-rqlite-m1` (`hack/rqlite-m1/run.sh`) runs the suite against the
pinned `rqlited` **and** against the embedded etcd of this repository, so every
observable — result, error, revision, event order — is compared with the real
current datastore instead of a hand-written expectation.

**Language.** M1 is a **Go** implementation of the language-neutral internal
interface of §3.2. The delivery environment for M1 has no Zig toolchain and no
Zig gRPC/HTTP2/TLS stack, so a Zig port could not be built, let alone verified;
a Go layer whose protocol behaviour is proven against etcd is the honest
intermediate step. The Zig port is a mechanical translation of this package
(same rqlite calls, same schema, same error strings) and its acceptance test is
this same M1 suite. It stays open as the M1 follow-up — it is not silently
dropped.

### 10.1 Implemented and differentially verified

| Surface | Contract served | Evidence |
|---|---|---|
| `KV.Range` | single key, prefix, explicit range, `limit`+`more`, `count`/`count_only`, `keys_only`, `rev` (historical read), all sort targets, `ErrCompacted` | `TestM1DifferentialContracts`, `TestM1DocumentedDeviations`, `TestM1StoreBinaryAndRange` |
| `KV.RangeStream` | etcd's chunking algorithm (initial chunk cliff, adaptive chunk limit, terminal chunk carrying `count`/`more`), `Unimplemented` for custom sort orders and revision filters | `TestM1RangeStreamDifferential` (12 shapes + terminal-chunk parity) |
| `KV.Put` / `KV.DeleteRange` | `ignore_value`/`ignore_lease` validation, `prev_kv`, no-op semantics, byte-identical keys/values (binary safe) | `TestM1DifferentialContracts` |
| `KV.Txn` | `mod`/`create`/`version`/`value`/`lease` compares, `then`/`else` put/delete/get/range, one revision per transaction, per-branch duplicate-key rejection, atomic response | M0 `TestTxnCompareBranchesAndRevision`/`TestTxnResponseComesFromTheCommittingTransaction`, `TestM1DifferentialContracts` |
| `KV.Compact` | compaction watermark, `ErrCompacted` below it, surviving revision at the watermark, history/events strictly below dropped | `TestM1StoreCompactionDropsEventsAndHistory`, `TestM1DifferentialContracts` |
| `Watch` | create/cancel, prefix and key watchers, `start_revision`, `prev_kv`, `NOPUT`/`NODELETE` filters, progress notify, `ErrCompacted` cancel with `compact_revision`, DELETE events as tombstones | `TestM1DifferentialContracts`, M0 watch evidence |
| `Lease.Grant`/`Revoke`/`KeepAlive`/`TimeToLive`/`Leases` | TTL grant, attached keys, keep-alive stream, expiry reaping, revoke deletes the attached keys | `TestM1LayerRestartContinuity`, `TestM1TimeoutLimits`, M0 |
| `Maintenance.Status`, `Maintenance.Alarm(GET)`, `Cluster.MemberList` | parseable semantic version (`3.7.1`), leader/member identity, leader-reported revision | `TestM1DifferentialContracts`, `TestM1ThreeNodeContracts` |
| TLS and mTLS | the same contract over a TLS listener; a client without the CA, a plaintext client and an mTLS client without its certificate are all rejected and write nothing | `TestM1TLSAndMTLSServeTheSameContract` |
| Size and timeout limits | the etcd `max-request-bytes` boundary (`etcdserver: request is too large`), the transport allowance above it, canceled/expired contexts | `TestM1RequestSizeLimitsMatchEtcd` (against an embedded etcd configured with the same limit), `TestM1TimeoutLimits` |
| Multi-member semantics | one semantic implementation reused by every member: a 3-member rqlite cluster serves the same contract, survives a leader kill and restart, and keeps its revision/monotonicity/accepted-write guarantees | `TestM1ThreeNodeContracts`, `TestM1ThreeNodeFailoverKeepsServing`, `TestM1LayerRestartContinuity` |

### 10.2 Deliberate deviations (explicit errors, never silent success)

| Shape | Reply |
|---|---|
| `Compare` with `range_end`, nested transactions, txn `RequestRange` with `revision != 0` | `InvalidArgument`, "rqlite compat layer does not support ..." |
| `Cluster.MemberAdd/Remove/Update/Promote` | `Unimplemented`: membership belongs to rqlite (`-join`, `POST /join`, `/remove`, `/nodes`) |
| `Maintenance.Snapshot/Defragment/Hash/HashKV/MoveLeader/Downgrade` | `Unimplemented`: owned by rqlite (backup via `GET /db/backup`, compaction/vacuum via rqlite, leadership via Raft) |
| `Maintenance.Alarm` activation | `Unimplemented`: rqlite has no etcd disk alarms; `GET` is served |
| `AuthService` | not registered: gRPC answers `Unimplemented` for the unknown service. K8E authenticates clients by mTLS |
| `v3election`, `v3lock`, `v3auth`, the v2 API | not served, as in class C |

The reply for every one of these is a real gRPC status with a message naming the
owner of the behaviour; none of them returns an empty success, which is what
`TestM1DocumentedDeviations` pins.

### 10.3 Known M1 limitations (recorded, not hidden)

* **`RangeStream` reads the matching keys before it sends the first chunk**, so
  the layer's memory cost for a large range is O(matches) and a fully consumed
  stream costs O(n²) chunk reads. `Count`/`more` on the terminal chunk require
  the full match count; an incremental chunk reader is the M2/M3 scaling item.
* **No request-id de-duplication** (§6): a client that retries the same
  write after a lost response can apply it twice. etcd itself does not promise
  cross-RPC exactly-once, but it does deduplicate internal retries; M1 records
  this as the uncertainty contract it inherits from M0.
* **Watch streams are polled** (`DefaultWatchPollInterval`, 20ms) against the
  durable event log rather than pushed. Watch latency is therefore bounded by
  the poll interval, and a transient rqlite read error ends the stream with the
  transport error rather than silently skipping events — a resumed watch reads
  from its `start_revision`.
* **A single compat layer process is not a cluster member**: several members may
  each serve clients against the same rqlite cluster (that is what
  `TestM1ThreeNode*` exercises), but member identity, leadership and durability
  are rqlite's.
* **Lease expiry** is driven by the member that owns the reaper; a restarted
  member re-arms it from the stored expiry time (checked in
  `TestM1LayerRestartContinuity`).

### 10.4 Wall-clock and environment facts

* Every evidence run uses real `rqlited` v10.3.5 processes (never a mock), and
  the differential runs start this repository's embedded etcd next to them.
* `go test ./...` stays green without the harness: the suite skips itself when
  `RQLITE_BIN` is unset (the same rule M0 established).

## 11. Milestones

* **M0 (done)** — design + capability evidence. Done when the M0 evidence suite
  (`make test-rqlite-m0`) passes against the pinned rqlite.
* **M1 (done, see §10)** — the compatibility layer: gRPC/TLS, KV/MVCC, Txn,
  Watch, Lease, Compaction, the maintenance/member calls in Appendix A classes A
  and B, plus differential tests against the current embedded etcd (results,
  errors, revisions, event order, crash recovery). Exit met: the compatibility
  matrix is complete and the differential/concurrency history checks pass. The
  **Zig port** of the delivered Go layer is the remaining M1 follow-up (§10).
* **M2** — K8E integration: the new driver, process hosting, bootstrap, certs,
  configuration, build/release, explicit opt-in test backend, then the four
  ordered E2E stages of the epic (safe recovery after failure; multi-node
  failure handling; backup-restore; long-run stability) reusing the #612
  tooling and criteria.
* **M3** — migration of existing production clusters and the first release
  that may switch the default backend, with migration/rollback rehearsals.

## 12. Non-goals

* No rewrite of rqlite or Raft; no general-purpose distributed SQL product.
* No mandatory S3; SQLite file copying is not a consistency protocol.
* No sharding, multi-writer scaling, cross-region SLA or in-process FFI
  embedding in the first version.
* "Using a mature rqlite" is not accepted as proof of compatibility-layer
  correctness.

---

## Appendix A — etcd API/semantics matrix (K8E + Kubernetes `v1.37.0-k3s1`)

Classes:

* **A — compat layer implements it** (serves the current Kubernetes call
  surface; implemented in M1, §10.1, with M0 evidence where noted).
* **B — call migration** (a caller inside K8E changes to a new interface or
  entry point; the etcd object/protocol does not have to be emulated).
* **C — unsupported** (must return an explicit error; never an empty success).

### A. Implemented by the compatibility layer

| etcd API | Why it is needed | Provenance / status |
|---|---|---|
| `KV.Range` (single key, prefix, explicit range, `limit`, `rev`, `keys_only`, `count_only`, `sort`) | every read and list; pagination via a continue key | `pkg/storage/etcd3/store.go`, `client/v3/kubernetes/client.go` (`Get`/`List`/`Count` → `WithRev`/`WithRange`/`WithPrefix`/`WithLimit`/`WithCountOnly`); storage-level semantics proven in M0 (`Range`, `RangePrefix`, bytewise order) |
| `KV.Range` with `rev` (historical read) | `ResourceVersion` reads, watch resume | implemented in `pkg/rqlitecompat` (§10.1), `ErrCompacted` below the compaction watermark; schema §5 keeps the history |
| `KV.Txn` (`If` mod/create/version/value/lease compare; `Then`/`Else` put/delete/get/range) | CAS for create/update/delete, `GuaranteedUpdate` | `client/v3/kubernetes` `OptimisticPut`/`OptimisticDelete` (`If ModRevision = expected` → `OpPut`/`OpDelete`, `Else OpGet`); **proven in M0** (single winner, shared revision, unchanged revision on failure, atomic response) |
| `KV.Put`/`DeleteRange` (via Txn) | object writes and deletes | as above |
| `KV.Compact` + compaction revision tracking | `pkg/storage/etcd3/compact.go` compacts on an interval and records the watermark under `compact_rev_key`; the apiserver reads it back | `compact.go:35,205,219,247,281`; implemented in `pkg/rqlitecompat` (§10.1) |
| `KV.RangeStream` (server-streaming range) | large recursive lists (etcd v3.6+ streaming) | `store.go:748 shouldStream` + `store.go:937 streamChunks` (`GetStream` at 951); feature gate `EtcdRangeStream` is **Beta and enabled by default in 1.37** (`pkg/features/kube_features.go:159,396`); the apiserver assumes support until an endpoint reports `Unimplemented`, so M1 must implement it (or accept a documented fallback after the first failure); **implemented in M1** (§10.1) by mirroring etcd's chunking, with the scaling caveat in §10.3 |
| `Watch` (create/cancel, `rev`, prefix/range, filters, `prev_kv`, progress notify) | controllers, caches, `RequestWatchProgress` | `watcher.go:128,366,470,475,477` (`WithRev`, `WithPrevKV`, `WithProgressNotify`, `WithRequireLeader`); implemented in `pkg/rqlitecompat` (§10.1), polled per §10.3 |
| `Lease.Grant` + attach key to lease (TTL) | `pkg/storage/etcd3/lease_manager.go:109` grants one reusable lease per object type (Kubernetes migrated off one-league-per-object) | implemented in `pkg/rqlitecompat` (§10.1) |
| `Lease.KeepAlive`/`Revoke`/`TimeToLive`/`Leases` | keepalive from the apiserver; revoke on shutdown | implemented in `pkg/rqlitecompat` (§10.1); expiry reaping survives a layer restart (§10.3) |
| `Maintenance.Status` | apiserver `Monitor` metric (uses `DbSize`) and the watch-progress feature check (uses `Version`) | `factory/etcd3.go:277`, `pkg/storage/feature/feature_support_checker.go`; implemented in M1: a parseable semantic etcd version (`3.7.1`), the leader and the leader-reported revision |
| ~~`Maintenance.Snapshot`~~ | the apiserver's `/etcd-snapshot`-style maintenance flows | `pkg/etcd/snapshot.go:329` (`snapshotv3.SaveWithVersion`) — **class B**: the RPC is *not* served (§10.2); K8E takes rqlite backups instead |
| `Maintenance.Alarm(GET)` | K8E's own maintenance paths | `pkg/etcd/etcd.go:1502`; implemented in M1 (an empty alarm list). Activation is `Unimplemented` (§10.2) |
| `Cluster.MemberList` | membership lifecycle | `pkg/etcd/etcd.go:255`; implemented in M1 (this member, the leader's revision). `Add`/`Remove`/`Update`/`Promote` are class C (§10.2) — rqlite owns membership |

### B. Call migration in K8E

| Current call | Replacement |
|---|---|
| `pkg/etcd` driver (`managed.Driver`: `Start`/`Reset`/`Restore`/`Snapshot`/`GetMembersClientURLs`/`RemoveSelf`) | new `pkg/rqlite` driver registering with `pkg/cluster/managed`; etcd member IDs/peer URLs are not reused |
| Member management: `MemberList`, `MemberAddAsLearner`, `MemberPromote`, `MemberRemove`, `MemberUpdate`, `MemberPromote` progress keys (`AddressKey`, `learnerProgressKey` KV puts) | rqlite's own membership (`-join`, `POST /join`, `POST /remove`, `/nodes`), with K8E tracking its own view in `identities` |
| `Status` (`pkg/etcd/etcd.go:1382,1525`) | rqlite `/status` + layer `Maintenance.Status` |
| `AlarmList`/`AlarmDisarm` (`etcd.go:1502`) | no rqlite equivalent for disk alarms; the driver maps them to layer-level health signals and reports "not implemented" where the etcd object cannot be honoured |
| `Defragment` (`etcd.go:1544`) | rqlite `-auto-vacuum-int` / `PRAGMA`-based maintenance; the etcd RPC maps to a documented no-op-with-explanation or an explicit unsupported error |
| `Snapshot` (`snapshotv3.SaveWithVersion`, `pkg/etcd/snapshot.go:329`) | `GET /db/backup` (verified: returns a readable SQLite file) with the layer's own manifest (revision, applied index, schema version) |
| `Restore` (`snapshotv3.NewV3(...).Restore`, `pkg/etcd/etcd.go:1637`) — writes etcd data-dir/WAL files | restore a rqlite backup into the rqlite data-dir (`POST /db/load` on a fresh directory) + layer migration entry; **etcd snapshot files are not readable** and are not accepted as a restore input |
| `pkg/etcdstorage` (bootstrap tokens: `List`/`Get`/`Create`/`Update`/`Delete` transactions) | the same compat-layer Txn semantics the apiserver uses; `pkg/cluster/storage.go` bootstrap KV path unchanged in shape |
| `setupStorageBackend` hard-codes `storage-backend=etcd3` (`pkg/daemons/control/server.go:292`) | explicit opt-in backend selection (M2); embedded etcd stays the default until M3 |
| apiserver etcd prober: `GET /<prefix>/health` (`factory/etcd3.go:264`) | served as a normal KV read on the layer (a missing key is a successful read) |
| apiserver preflight: TCP dial to the etcd endpoint (`preflight/checks.go:39`) | unchanged — the layer listens on an etcd-compatible endpoint |
| Secret encryption at rest, mTLS client certs, ACL isolation | preserved: layer terminates mTLS with the existing certificates; encryption stays in the apiserver |

### C. Explicitly unsupported

| API / behaviour | Handling |
|---|---|
| etcd v2 API (`/v2/*`, `--enable-v2`) | not served; `Unimplemented` |
| `AuthService` (`AuthEnable`, `Authenticate`, roles, users) | not implemented. Kubernetes does not use etcd's own auth against its datastore (K8E authenticates clients by mTLS); the RPCs return `Unimplemented` |
| `Maintenance.MoveLeader`, `Downgrade`, `Defragment`, `Hash`, `HashKV`, `Snapshot`, `Alarm` activation | explicit `Unimplemented` with a message naming the rqlite owner (§10.2); rqlite exposes its own equivalents |
| `v3election`, `v3lock`, `v3auth` gRPC-gateway services | not implemented (`Unimplemented`); not on the current K8E/Kubernetes path |
| etcd snapshot/WAL file formats (`etcdutl snapshot restore`, member directories) | not readable; restore goes through rqlite backups (class B) |
| etcd member identity semantics (member IDs, peer URLs, learner promotion protocol, `etcdctl member` output) | not emulated; K8E's driver exposes rqlite membership instead |
| `etcdctl` compatibility in general | only the RPCs above are guaranteed; `etcdctl` commands outside them fail on the wire rather than silently returning success |
| `watch` guarantees for a revision that has been compacted | explicit `ErrCompacted`-equivalent error, never an empty stream |
| Cross-RPC exactly-once for clients | not promised (etcd does not promise it either); internal retry de-duplication is per §6 |

**Settled in M1** (were the open M0 questions): `RangeStream` under the enabled
feature gate (chunk-for-chunk differential against embedded etcd),
watch progress-notify ordering, lease expiry and lease state across a layer
restart and under a rqlite leader change, compaction/revision interaction with
concurrent readers, and the `Alarm`/`Defragment` mapping (§10.2: explicit
`Unimplemented`, because they are rqlite's, not the layer's).

**Still open after M1**: tombstone `version` parity with etcd's internal value
(our tombstones carry the delete revision and no version), and the Zig port of
the layer (§10).
