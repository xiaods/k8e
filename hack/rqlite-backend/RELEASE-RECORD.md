# rqlite + etcd v3 backend — release acceptance record

| | |
|---|---|
| Issue | [#619](https://github.com/xiaods/k8e/issues/619) (epic [#592](https://github.com/xiaods/k8e/issues/592)) |
| Design | [KIP-29](../../docs/kip-29-rqlite-etcd-compat-backend.md) |
| M0 evidence | [tests/rqlitecompat](../../tests/rqlitecompat/) via [`hack/rqlite-m0/run.sh`](../rqlite-m0/run.sh) (`make test-rqlite-m0`) |
| Contract check | `go test ./tests/rqlitebackend/` |
| Status | **not released — the acceptance gate is not satisfied** |

This is the release record required by completion criterion 5 of Issue #619: it
pins the exact versions, states what was tested, names what was **not** tested
and the known limits, and keeps the four non-goals binding. It is a contract,
not a narrative: `go test ./tests/rqlitebackend/` fails when a required section
is missing, when one of the four non-goals or one of the five completion
criteria disappears, when a criterion claims `met` without an evidence pointer,
or when a relative link is broken.

## 2. Exact versions

| Component | Version | Pinned identity |
|-----------|---------|-----------------|
| Kubernetes | v1.37.0-k3s1 | KIP-26 baseline; `k8s.io/kubernetes` replace in `go.mod` |
| etcd (current default backend, compatibility baseline) | v3.7.1-k3s1 | embedded etcd in `pkg/embedw`; the behaviour this work must match |
| rqlite | v10.3.5 | commit `58f9d8898a5f5d4bf712d472f96e38cc7cb4748a`; linux-amd64 sha256 `edaef7580cd60f7f9d558cde2fb95011f6e356ff64da7167fb659ab4b6882014`; linux-arm64 sha256 `23d5d09d34a9e2db54b2c21469aacb3b6563166e3d0d3901f4f4fb44990db99b` |
| Go (M0 prototype and K8E driver) | go1.26.7 | `go` directive in `go.mod` |
| Zig (M1 compatibility layer) | 0.16.0 | pinned in `.github/workflows/testing.yml`; the layer is not started |

An upgrade of any row above re-runs the evidence below on the new version before
the record changes; the rqlite upgrade surface is the endpoint table in
[KIP-29 §7](../../docs/kip-29-rqlite-etcd-compat-backend.md) plus the schema in
§5.

## 3. Non-goals (binding)

- 不重写 rqlite/Raft，不打造通用分布式 SQL 产品。
- 不引入强制 S3，不把 SQLite 文件复制当成数据库一致性协议。
- 首版不做分片、多写者扩展、跨地域 SLA 或同进程 FFI 嵌入。
- 不用“使用成熟 rqlite”代替 Zig 兼容层的正确性证明。

## 4. Completion criteria and gate status

Status values: `met` (satisfied with the named evidence), `not-met` (evidence
exists but the criterion is not satisfied), `not-executed` (the scenario has
not been run at all). A `met` row must carry a non-empty evidence pointer.

| ID | Completion criterion | Status | Evidence / blocker |
|----|----------------------|--------|--------------------|
| C1 | A user who installs K8E starts a single node with no separately deployed database or object store | not-executed | gated by [#617](https://github.com/xiaods/k8e/issues/617); the K8E driver and process hosting do not exist |
| C2 | A three-member deployment, fault recovery, backup/restore and the member lifecycle pass acceptance | not-executed | gated by [#616](https://github.com/xiaods/k8e/issues/616) and [#617](https://github.com/xiaods/k8e/issues/617); no compatibility layer and no cluster E2E harness |
| C3 | The protocol semantics Kubernetes/K8E requires and production workloads are verified with no open data-correctness defect | not-met | M0 proves storage-level semantics against a real `rqlited` in [tests/rqlitecompat](../../tests/rqlitecompat/); the gRPC layer, the differential tests and the production workloads are not built |
| C4 | Migration and rollback have real drill evidence and the first release can carry an existing production cluster | not-executed | gated by [#618](https://github.com/xiaods/k8e/issues/618); the first version is a stop-write migration in a maintenance window |
| C5 | The release record names exact versions, test scope, unexecuted scenarios and limitations, and does not treat “the process starts” as completion | met | this document, enforced by `go test ./tests/rqlitebackend/` |

## 5. Executed test scope

- M0 evidence suite: `make test-rqlite-m0` fetches and SHA-256-verifies
  `rqlited v10.3.5`, then runs [tests/rqlitecompat](../../tests/rqlitecompat/)
  against it — atomic Txn (compare branches, revision allocation, an
  atomically-consistent response), exactly-once replay of a lost response,
  explicit `level=linearizable` reads, bytewise BLOB ordering, crash
  durability, concurrent adapter processes, a leader SIGKILL, and the bounded
  no-quorum failure.
- Embedded etcd robustness baseline phase 1 (Issue #612):
  [tests/etcdrobustness](../../tests/etcdrobustness/) exercises the *current*
  backend so the replacement has a baseline, not only a target.

## 6. Unexecuted scenarios

- M1: the Zig gRPC/TLS compatibility layer and its differential tests against
  embedded etcd ([#616](https://github.com/xiaods/k8e/issues/616)).
- M2: the K8E driver, bootstrap, certificates, configuration and the four
  ordered E2E stages — fault recovery, multi-node faults, restorable backups,
  long-run stability ([#617](https://github.com/xiaods/k8e/issues/617)).
- M3: the production-equivalent migration/rollback drill and the operator
  runbook ([#618](https://github.com/xiaods/k8e/issues/618)).
- arm64: the digest is published but the M0 suite has not run on arm64
  hardware.
- A real `kube-apiserver` with CRDs, RBAC, Secrets, Pods and the sandbox
  lifecycle on the new backend.
- The embedded-etcd comparison: RSS, CPU, I/O, API/Watch tail latency,
  recovery time and package size.
- The optional S3 backup target; S3 unconfigured must not affect startup or
  writes.
- VM power loss and ENOSPC/EIO on the rqlite data directory.

## 7. Limitations

- The M0 prototype is Go and proves storage-level semantics, not the Zig
  compatibility layer; “rqlite is mature” is not correctness evidence (non-goal
  4).
- No watch, lease, compaction, membership or TLS behaviour on the new backend
  is proven.
- Only rqlite v10.3.5 is exercised; a version upgrade re-runs the suite above.
- etcd snapshot/WAL files are not readable by rqlite; restore goes through
  rqlite backups instead.
- This record covers the versions in §2 only; it is not a capacity or
  performance report.

## 8. Release gate

The default backend stays embedded etcd, and no rqlite-backend release is cut,
until C1–C4 each have executed evidence. “The process starts” is not
completion: a criterion is `met` only with the evidence named in the table
above. When an upgrade or a base advance changes a pinned version or a status,
that change and its evidence land in the same commit as this record.
