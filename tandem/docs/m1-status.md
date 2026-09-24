# Tandem M1 status

Issue #616 defines M1 as a real etcd v3 compatibility layer, not a protocol
type catalogue. The implementation must satisfy all of these before M1 can be
called complete:

| Area | Current status | Required next step |
| --- | --- | --- |
| gRPC/TLS transport | Linux TLS/mTLS Watch integration validated | Keep official-client interoperability in CI; document io_uring runtime requirements |
| KV/MVCC | production persistence connected | Startup requires rqlite and restores revision/compaction; KV and history survive restart. Complete concurrent/differential semantics validation |
| Txn | transaction-local Range answers from the committing transaction | Validate the remaining compare targets/options and the previous-delete response shape against etcd. Nested transactions are outside the promised surface (Appendix A scopes Txn to put/delete/get/range) and are refused with `Unimplemented`, which is the required handling for a call the layer does not serve |
| Watch | single-process Linux integration validated | Live transaction events, historical replay, filters, cancellation and compaction errors are covered; validate cross-process fanout, slow consumers and recovery |
| Lease | persistent grant, keepalive, expiry and lease-driven deletes | The reaper still runs on request rather than on a timer, and no single-expiry-owner is elected; both are required before failover |
| Compaction | watermark and historical-read boundary enforced | Verify interaction with concurrent readers and the SQL history/space reclamation path |
| Maintenance | Status reports real db size and a parseable version; Snapshot returns an explicit `Unimplemented` | Alarm/Defragment/Hash remain constant-valued; decide each mapping or return an explicit error |
| Single/three member | single node only | Share one semantic engine and add rqlite cluster lifecycle management |
| Differential tests | incomplete | Compare etcd and Tandem results, errors, revisions and event order |
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
container. On the same run all 23 tests pass.


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
  empty deletes, and concurrent CAS.
- Independent Pipeline processes contend on one rqlite using CAS; exactly one
  of eight wins. This does not qualify three-node lifecycle or failover.

The L1 CI job also runs the tagged Tandem integration suite against its pinned
rqlited and built Tandem binary, so persistence and TLS regressions no longer
depend solely on manual runs. CI must pass on the pushed head before claiming
Linux amd64 qualification.

Remaining merge gates: complete compare/options semantics and the
previous-delete response shape; a background lease reaper with a single
expiry owner; multi-node join/readiness/failover; removal of embedded-etcd
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
