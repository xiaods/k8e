# Embedded etcd robustness E2E — design and phased rollout

Status: **phase 1 landed** (fault-recovery safety, `tests/etcdrobustness`).
Phases 2–4 are designed here and **not implemented yet**.

Related: [KIP-6](../../../docs/kip-6-embedded-etcd-design.md) (embedded etcd as
sole datastore), [KIP-7](../../../docs/kip-7-embedded-etcd-fuse.md)
(`pkg/embedw` fuse), [hack/e2e](../run.sh) (the container-level E2E harness).

## 1. Goal and user outcome

K8E runs Kubernetes on an embedded etcd (`pkg/embedw` over
`embed.StartEtcd`). An operator whose node loses power, whose disk fills up or
whose WAL is damaged must be able to answer one question with evidence:

> after the fault, are the writes Kubernetes acknowledged still there, and is
> the datastore usable again?

This program makes that question executable. Every phase adds scenarios that
inject a real fault into a real etcd and then check the recovered state against
an **external operation history** recorded before the fault, so "it looked fine
afterwards" is never accepted as evidence.

The user outcome per phase:

| Phase | User-visible promise |
|-------|----------------------|
| ① fault-recovery safety | after a restart/kill/disk fault the acknowledged writes survive, the member keeps its identity, and the store is writable again |
| ② multi-node fault handling | killing or partitioning one member of a 3-member cluster does not lose acknowledged writes, and the cluster converges when it returns |
| ③ backups actually restorable | a snapshot taken before a fault restores into a working datastore with the acknowledged writes |
| ④ long-run stability | a long mixed workload (compaction, defrag, restarts) keeps the store healthy: no unbounded growth, no revision rollback, no lost write |

Failure path (same for every phase): the suite fails the specific scenario, and
the retained work directory (`etcd.log`, `history.jsonl`, data dirs) shows the
fault, the acknowledged history and the recovered state side by side, so the
gap is diagnosable without reproducing the fault by hand.

Ordering is deliberate: ① establishes the recording + oracle machinery and the
single-member recovery baseline; ② reuses both on a cluster; ③ proves the
snapshot is worth having before ④ runs long enough for snapshotting to matter.
Doing ③ before ① would test restores of a store whose fault behaviour is
unknown.

## 2. What already exists

* `pkg/embedw` starts an embedded etcd from a data directory and returns once
  `ReadyNotify()` fires (60s limit, `pkg/embedw/etcd.go:93`).
* `pkg/etcd` owns the K8E-level lifecycle: `Start`, `Test` (defragment + clear
  alarms on startup, `pkg/etcd/etcd.go:211`), snapshots, learners, `Restore`.
* `hack/e2e` brings up server + agent containers and runs `suites/l1.sh` /
  `suites/l2.sh`, including one Go test that restarts a container
  (`pkg/sandboxmcp/cluster_test.go`).
* There is no etcd fault-injection, no external operation history and no
  restore verification anywhere in the tree.

### 2.1 Topology, runner capability, budgets and CI layering

Three runner layers exist, and a result is only evidence for the layer it ran
on:

| Layer | Faults it can inject | Scenarios |
|-------|----------------------|-----------|
| in-process (`go test`, this package) | process kill, restart on the same data dir, WAL damage, quota exhaustion | phase 1 (landed) |
| container (`hack/e2e/run.sh`) | K8E server stop/kill, network partitions, a size-limited dedicated volume | phase 2+, plus the container layer of phase 1's strong-kill row |
| dedicated VM | host reboot, hard power-off, block-device EIO | phase 1's power-loss row — **not executed** |

* **Topology.** Each member owns its own data directory, client and peer
  address; restarting against the same data directory must recover that member
  and never bootstrap a second cluster (`StartNode` asks for `existing` when
  `member/snap` exists). Phase 2 grows the same `NodeOptions` to the member
  list. A container restart or a SIGKILL is *not* power-loss durability and is
  never reported as one; the power-loss row stays unexecuted until a VM runner
  exists.
* **Runner capability.** The in-process layer runs the repository's own
  `pkg/embedw` at the commit under test, so the artifact under test is pinned by
  the commit and `go.sum`; the workload seed is fixed (`defaultSigkillSeed`) and
  the strong-kill dwell is derived from it, so a run is reproducible. The
  container and VM layers must additionally pin the K8E binary SHA, the image
  digest and a binary matching the Docker daemon
  (`hack/e2e/build-linux.sh`) before their results count.
* **Disk faults never touch the host.** ENOSPC and EIO are injected on a
  dedicated size-limited filesystem (or an explicitly labelled equivalent) and
  never by filling the machine's disk. Phase 1 writes every work directory under
  `ETCD_ROBUSTNESS_WORKDIR` and damages a *copy* of the data directory.
* **Budgets are test ceilings, not production SLAs**, and are never relaxed
  after a failure to make it green: 90s for the graceful-restart scenario
  (including the recovered member's CRUD/Watch check), 60s per strong-kill round
  (default 10 rounds), 180s for the quota scenario, 60s for a damaged-WAL
  start. Every wait is a condition poll with a total deadline (`waitForFile`,
  `waitForAcknowledged`, `collectEvents`, the per-test context), never a fixed
  sleep that decides recovery, so a hung member or a lost quorum fails inside
  the budget instead of hanging the job. These are the in-process ceilings; the
  K8E-server API-readiness budget in the container layer starts from the Issue's
  300s and is tightened only with measurements.
* **CI layering.** Today the PR job is the only layer: `go vet ./...` plus
  `go test ./...` (`.github/workflows/testing.yml`) runs the deterministic
  phase-1 subset below on every PR. The later layers are designed but not wired:
  a nightly job for repeated multi-member and restore scenarios, and a dedicated
  VM job for power-loss, EIO and the 24h soak. Each must gate on its own
  prerequisites and fail when they are missing, never silently skip, so a green
  PR is not read as a robustness pass.

## 3. Harness: external operation history + oracle

Two pieces, both in `tests/etcdrobustness`, both phase-independent:

**`history.go` — the operation history.** Every mutating request is written to
`history.jsonl` *outside* the etcd data directory, with a fsync: first a
`pending` intent carrying the payload hash (started before the request is
issued), then one terminal record. The recorder runs in the process that drives
the workload, which is not the process hosting the member under test, so the
fault cannot take the record of an acknowledged response with it: a response
that arrived before the kill stays `acknowledged` instead of degrading into an
`unknown` the oracle would excuse.

| Outcome | Meaning |
|---------|---------|
| `acknowledged` | the server returned a response; the revision is recorded |
| `rejected` | the server explicitly refused (e.g. quota exceeded) |
| `unknown` | the client never learned the outcome (timeout, connection break, process death) — a lone `pending` record replays as `unknown` |

`unknown` is not a failure. It is the honest state of a request that may have
been committed after the client stopped waiting, and the oracle must explain it
both ways.

**`oracle.go` — the correctness oracle.** Given the history and a reader for the
recovered state, `Verify` checks:

1. *Every key is explainable.* The allowed state starts from the effect of the
   highest-revision acknowledged mutation (absent when nothing was acked) and
   additionally allows the effect of each `unknown` operation on that key, then
   requires the observed payload hash to be one of them. Anything else is a
   violation: a value nobody acknowledged, a lost acknowledged write, or a
   resurrected delete.
2. *At most one concurrent CAS wins.* Two acknowledged CAS on the same key and
   expected revision would mean the compare-and-swap was not really applied.
3. *Revision continuity.* The recovered revision must not be below the highest
   acknowledged revision (a rollback would prove a lost commit or a silently
   recreated cluster).

Phase 1 feeds the reader straight from a real client (`ClientReader`); phases
2–4 add readers for cluster members and for a restored store, and reuse the same
oracle unchanged.

## 4. Phase 1 — fault-recovery safety (landed)

Package: `tests/etcdrobustness` (test support; no shipped binary imports it).

| Scenario | Test | Fault |
|----------|------|-------|
| graceful member restart | `TestEmbeddedEtcdGracefulRestart` | stop and start on the same data dir |
| strong kill while writing | `TestEmbeddedEtcdSigkillDuringWrites` | repeated SIGKILL of a child member under continuous acknowledged writes (fixed seed, default 10 rounds; `TestEmbeddedEtcdRobustnessChild` hosts only the member, while the workload and the recorder stay in the parent) |
| disk pressure | `TestEmbeddedEtcdQuotaExhaustionRefusesWrites` | fill the 8MB quota until writes are refused with `mvcc: database space exceeded` |
| damaged WAL | `TestEmbeddedEtcdWALCorruptionIsRefused` | flip bytes in the written region of a WAL copy at two damage sites |
| history/oracle contract | `history_test.go`, `oracle_test.go` | unit level: record reduction, `unknown` replay, every oracle rule and its violation |

What each scenario asserts (the real contract, not the implementation's guess):

* graceful restart — member id, cluster id and revision survive, no oracle
  violation, and CRUD/CAS/Watch still work on the recovered member; eight
  concurrent CAS on one key produce exactly one winner at every stage.
* strong kill — no error on the next start beyond the kill itself, member and
  cluster identity unchanged, every acknowledged record still present, no
  violation and no revision rollback; rounds where an in-flight request became
  `unknown` must be explained by the oracle (the rounds log how many). The
  recorder lives in the parent process, so a write the member acknowledged
  before the kill is in the history as `acknowledged` and a lost one cannot hide
  as an explainable `unknown`.
* disk pressure — the refusal is `database space exceeded` with an active
  `NOSPACE` alarm, the acknowledged data is still intact under the oracle, and
  after delete → compact → defragment → disarm (the same repair
  `pkg/etcd/etcd.go:211` performs at startup) the alarm is gone and the store
  accepts a new write.
* damaged WAL — the node must not come up serving a store; a node that starts
  is a failure. The two deterministic damage sites fail differently on
  `etcd 3.7.1-k3s1` (see findings) and both are recorded. The untouched data
  directory still starts and still holds all 50 records, which proves the test
  damaged only the copy.

### 4.1 Phase-1 coverage: executed, not executed, unsupported

The phase-1 table in the Issue names six scenarios. The strong-kill row is
split here into its embedded-etcd and K8E-server layers, because the Issue's
note requires the K8E recovery budget to measure bootstrap and API readiness
and the in-process layer cannot, so this landing reports seven statuses instead
of implying the whole phase is done:

| Issue scenario | Status | Evidence / missing prerequisite |
|----------------|--------|---------------------------------|
| graceful restart | **executed** | `TestEmbeddedEtcdGracefulRestart` |
| SIGKILL during acknowledged writes, 10 fixed-seed rounds | **executed at the embedded-etcd layer** | `TestEmbeddedEtcdSigkillDuringWrites` (child member killed mid-write) |
| SIGKILL of the K8E server, recovery measured to API readiness | **not executed** — needs the container layer | phase-2 harness extension (§5); the in-process run does not exercise kube-apiserver, `pkg/etcd`'s startup repair or a recovery budget for the control plane |
| host reboot / hard power-off | **not executed** — needs the dedicated VM runner | declared here, no result claimed |
| backend quota exhausted | **executed** | `TestEmbeddedEtcdQuotaExhaustionRefusesWrites` |
| filesystem full / EIO | **not executed** — needs a size-limited dedicated volume | declared here, no result claimed; quota exhaustion is *not* a substitute |
| WAL/snapshot damage (negative case) | **executed for the WAL** | `TestEmbeddedEtcdWALCorruptionIsRefused`; snapshot damage lands with phase 3 |

What that means for reading the result: the in-process strong-kill round proves
WAL recovery, cluster-identity retention and the oracle on an acknowledged
history. It does **not** prove that the whole control plane returns within a
recovery budget, and nothing here is power-loss evidence. The scenario keeps
`unknown` outcomes honest (each round logs how many requests were in flight),
and the oracle's failure paths have their own controlled examples:
`TestVerifyDetectsLostAcknowledgedWrite`, `TestVerifyRejectsUnexplainedState` and
`TestVerifyDeleteIsNotResurrected` each make `Verify` fail on a seeded history,
so a violated oracle is a diagnosis and not a surprise.

Still open from the Definition of Done: the defects below (F1–F4) are recorded
disclosures, not fixes — they have no tracking issue and no regression case yet,
so the phase-1 checklist item "defects found by the tests have an explicit
issue/fix and a regression case" stays open.

Known coverage gaps of the landed scenarios (disclosed, not claimed covered):
the oracle compares a key's payload hash, not its per-key revision, so the
scenarios write a unique payload per request to keep versions distinguishable;
the post-restart check exercises Put/Get/CAS/Delete/Watch, but Watch is only
counted, not reconnected after a compaction; and every landed scenario proves
the embedded-etcd layer, never the K8E control plane.

### Running phase 1

```bash
go test ./tests/etcdrobustness/ -count=1 -v                      # full phase 1 (~30s)
go test ./tests/etcdrobustness/ -run 'Sigkill' -count=1          # one scenario
```

Environment knobs (all optional):

| Variable | Meaning |
|----------|---------|
| `ETCD_ROBUSTNESS_WORKDIR` | base directory for the per-test data dirs. A member preallocates a 64MB WAL and the disk-fault cases copy it, so point this at a large filesystem when the default temporary one is small. |
| `ETCD_ROBUSTNESS_KEEP=1` | keep every work directory, not only the ones of failed tests |
| `ETCD_ROBUSTNESS_ROUNDS=N` | number of strong-kill rounds (default 10) |
| `ETCD_ROBUSTNESS_SKIP_SIGKILL=1` | skip the strong-kill scenario |

A failing test retains its work directory and logs the path, with
`etcd.log` (the node's own log, kept outside the data directory),
`history.jsonl` and `fingerprint.json` inside it.

### Evidence from the landing run

```
TestEmbeddedEtcdGracefulRestart          PASS
TestEmbeddedEtcdSigkillDuringWrites      PASS (10 rounds; one in-flight request per round became unknown and the oracle explained it)
TestEmbeddedEtcdQuotaExhaustion...       PASS
TestEmbeddedEtcdWALCorruptionIsRefused   PASS
TestEmbeddedEtcdRobustnessChild          SKIP (child process only)
```

### Findings (phase-1 output, not tuned green)

**F1 — a damaged WAL makes `embed.StartEtcd` panic.** Corrupting bytes in the
first (metadata) record of a WAL makes etcd 3.7.1-k3s1 panic with a nil
pointer dereference inside `etcdserver.bootstrap` instead of returning an
error. A K8E server in this state crashes during startup. Reproduced 8/8.

**F2 — the WAL repair path rewrites the damaged segment first.** In the same
case the 64MB segment is truncated to 16 bytes before the crash, i.e. the
evidence is destroyed before anyone can inspect it. The test records the size
change instead of asserting it away.

**F3 — a mid-WAL byte flip is not fail-fast.** Corrupting the middle of the
written region usually gives a clean `walpb: crc mismatch`, but
non-deterministically produces a member that starts, elects itself leader and
then never becomes ready. `pkg/embedw`'s readiness wait then blocks the full
60s and returns a bare `context.DeadlineExceeded` with no mention of the WAL
(observed in 2 of 4 mid-WAL corruption runs; the failing byte offset depends on
the WAL length, and the other two runs refused cleanly). That is the least diagnosable outcome an operator can get, and it
is why the corruption test only locks the two deterministic damage sites.

**F4 — the reported backend size is not tied to the quota.** With an 8MiB
`quota-backend-bytes`, the sizes reported at the moment of refusal ranged from
8,327,168 to 9,338,880 bytes across six runs: etcd refuses the write that would
cross the quota, so `DbSize` sits either side of it. The test therefore logs the
size instead of asserting it. Capacity or alarm rules built on `DbSize` versus
the quota ratio — the numbers `pkg/etcd` logs on startup — would be misleading,
particularly since the backend can grow past the quota without being exhausted.

## 5. Phase 2 — multi-member fault handling (designed, not landed)

* Harness extension: start N (`NodeOptions.InitialCluster` already takes the
  member list) members in one process, one recorder per member plus a merged
  reader so the oracle checks the surviving cluster's state after a member
  dies.
* Scenarios: (a) kill one member of three during a mixed workload, restart it,
  verify no acknowledged write is lost and the member rejoins (identity kept);
  (b) kill two members, verify the cluster refuses writes rather than
  acknowledging them (no false ack), then verify recovery after both return;
  (c) learner promotion / `RemovePeer` interleaved with the workload, since
  `pkg/etcd` drives both; (d) a member whose data dir is behind (restart with
  the WAL of an earlier point) must fail to rejoin instead of silently
  diverging.
* The oracle is reused unchanged; only how many readers feed it grows.
* Container-level layer, once the in-process layer is green: reuse
  `hack/e2e/run.sh` to stop/start the server container (the pattern already in
  `pkg/sandboxmcp/cluster_test.go`) and rerun the same history/oracle checks
  against the real `k8e` binary's data dir. This layer owns the phase-1 row
  "SIGKILL of the K8E server measured to API readiness": the fault is a real
  `SIGKILL` (not a graceful stop), the history is written outside the container,
  and recovery is measured by an API call succeeding within the budget, not by
  the process being present.

## 6. Phase 3 — backups actually restorable (designed, not landed)

* Harness extension: a store-level scenario that takes a snapshot through
  `pkg/etcd`'s snapshot path (and `etcdutl` for the restore), then restores
  into a fresh data directory.
* Acceptance: the restored store's key state satisfies the oracle for every
  operation acknowledged **before** the snapshot, the restored revision equals
  or exceeds the snapshot revision, and the restored member is writable and
  watchable. A snapshot that restores an empty store must fail the scenario,
  not pass silently.
* Failure path: a corrupt/truncated snapshot file must make the restore fail
  loudly, leaving the target data directory untouched.

## 7. Phase 4 — long-run stability (designed, not landed)

* A long mixed workload (put/delete/CAS/Watch, periodic compact + defragment,
  restarts) driven by a seeded generator, with sampled checks:
  `DbSize`/`DbSizeInUse` stay bounded, the revision never rolls back, every
  sampled acknowledged write is present, and `Watch` from a captured revision
  still delivers every change.
* This is the phase where the phase-1 findings matter: a 60s hang (F3) or a
  panic (F1) found only after hours is far more expensive than one found by a
  deterministic unit-size scenario.

## 8. Non-goals

* No new daemon, database, queue or scheduler: the scenarios are Go tests, the
  history is a file.
* No Kubernetes-level fault injection beyond what phase 2 names.
* No performance benchmarking (latency/throughput) — this program measures
  safety and recoverability.
* No changes to `pkg/embedw`/`pkg/etcd` in phase 1: the phase-1 job is to
  establish the baseline and the evidence. F1–F4 are inputs for a follow-up
  decision (for example failing fast with a WAL-specific error instead of the
  60s readiness wait), not silently patched here.

F1–F3 share one actionable follow-up: `pkg/embedw` should distinguish "the
store is damaged" from "the member never became ready" and surface the WAL
error instead of a bare `context.DeadlineExceeded`, with a regression case
reusing `TestEmbeddedEtcdWALCorruptionIsRefused`'s damage sites.
