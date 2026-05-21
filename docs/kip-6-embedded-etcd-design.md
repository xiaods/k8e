# KIP-6: Embedded etcd — Remove kine/SQLite and harden embedded etcd

**Status**: Accepted
**Author**: xiaods
**Created**: 2025-05-20
**Relates to**: KIP-3 (Agentic AI Sandbox Matrix), KIP-4 (Sandbox MCP Skill)

---

## 1. Context and Problem Statement

K8E is a CNCF-conformant Kubernetes distribution packaged as a single binary under 100MB, purpose-built for secure, isolated AI agent execution at scale via sandboxed Kubernetes pods.

The upstream **k3s** project (Rancher) uses a **kine/SQLite** abstraction layer as its datastore:

```
kube-apiserver / kubelet / controllers
        │
        ▼
      kine (abstraction layer)
        │
   ┌────┴────┐
   │         │
 SQLite   etcd     ← k3s can use either backend
```

This kine layer allows k3s to swap between SQLite (embedded, single-node) and etcd (clustered) at runtime. However, for K8E's use case, this abstraction is unnecessary overhead.

### What K8E currently does

K8E already uses **embedded etcd** directly — not kine/SQLite:

```
kube-apiserver / kubelet / controllers
        │
        ▼
   embedded etcd (v3.6.7-k3s1)
```

The codebase has **zero runtime SQLite/kine code**. All datastore operations go through:
- `pkg/cluster/` — cluster bootstrap and managed DB driver (`managed.Driver` interface)
- `pkg/etcd/` — etcd lifecycle management (`ETCD` struct implementing `managed.Driver`)
- `pkg/etcdstorage/` — low-level KV client for bootstrap reconciliation

However, vestigial k3s artifacts remain in the build system, and the architecture should be formally documented and hardened.

## 2. Decision

**K8E will use embedded etcd as its sole datastore. All kine/SQLite code paths will be removed or disabled.** The key changes are:

1. Remove vestigial SQLite CFLAGS from `build.zig`
2. Hard-code `storage-backend=etcd3` in the kube-apiserver config (already done)
3. Formalize the `managed.Driver` interface with a single `etcd` implementation
4. Remove the `--disable-etcd` proxy mode or re-scope it as "external etcd" (not kine)
5. Document the architecture clearly for contributors

## 3. Architecture

### 3.1 Embedded etcd Data Flow

```
┌─────────────────────────────────────────────┐
│               k8e server                     │
│                                             │
│  ┌──────────┐    ┌──────────────┐          │
│  │ CLI Flags│───▶│ Config Merge │          │
│  └──────────┘    └──────┬───────┘          │
│                         │                   │
│                         ▼                   │
│              ┌─────────────────┐            │
│              │ cluster.Bootstrap│           │
│              │  (managed.Driver)│           │
│              └────────┬────────┘            │
│                       │                     │
│              ┌────────▼────────┐            │
│              │ etcd.ETCD       │            │
│              │ (embedded etcd) │            │
│              │  v3.6.7-k3s1    │            │
│              └────────┬────────┘            │
│                       │                     │
│         ┌─────────────┼─────────────┐      │
│         ▼             ▼             ▼      │
│  ┌────────────┐ ┌──────────┐ ┌──────────┐ │
│  │kube-apiserver│ │scheduler │ │CCM/other │ │
│  │(etcd3 backend)│ │          │ │          │ │
│  └────────────┘ └──────────┘ └──────────┘ │
│                                             │
│  ┌──────────────┐                          │
│  │ etcdstorage  │ ← bootstrap reconciliation│
│  │ .Client      │   (direct KV access)     │
│  └──────────────┘                          │
└─────────────────────────────────────────────┘
```

### 3.2 Key Components

| Component | Path | Role |
|---|---|---|
| `managed.Driver` | `pkg/cluster/managed/drivers.go` | Interface for datastore backends |
| `etcd.ETCD` | `pkg/etcd/etcd.go` | Single implementation of `managed.Driver` |
| `etcdstorage.Client` | `pkg/etcdstorage/client.go` | Low-level KV for bootstrap token storage |
| `cluster.Cluster` | `pkg/cluster/cluster.go` | Orchestrates bootstrap lifecycle |
| Storage config | `pkg/daemons/config/types.go:40-46` | Config struct for datastore connection |

### 3.3 Embedded etcd Lifecycle

1. **Server starts** → `pkg/server/server.go` → `control.Server()`
2. **Config prepared** → `pkg/daemons/control/server.go: prepare()` → defaults, certs, `cluster.New()`
3. **Bootstrap** → `cluster.Bootstrap()` → `assignManagedDriver()` → `managed.Default()` (= etcd)
4. **Start etcd** → `cluster.Start()` → `managedDB.Start()` → `etcd.Start()`
5. **Await readiness** → polls `etcd.Healthy()` every 5 seconds
6. **Storage bootstrap** → `startStorage()` → `etcdstorage.Client` writes bootstrap tokens
7. **API server starts** → uses `--storage-backend=etcd3` → connects to embedded etcd
8. **Remaining components** → scheduler, controller-manager, CCM start

### 3.4 External etcd (Proxy Mode)

The `--disable-etcd` flag allows K8E to connect to an **external etcd cluster** instead of running embedded:
- The embedded etcd process is NOT started
- All components connect to user-specified `--datastore-endpoint`
- Bootstrap tokens are read from external etcd via `etcdstorage.Client`

## 4. What Changes from k3s's Approach

| Aspect | k3s (upstream) | K8E |
|---|---|---|
| Datastore abstraction | kine (SQLite + etcd switchable) | Direct embedded etcd only |
| etcd version | v3.6.x-k3s1 (forked) | v3.6.7-k3s1 (forked) |
| Storage backend flag | `--cluster-backend` (kine or etcd) | Hard-coded `etcd3` |
| SQLite CFLAGS | Required for kine build | Removed |
| BoltDB usage | Via kine → SQLite | Via etcd internals only |
| Raft consensus | Optional (SQLite mode) | Always (embedded etcd) |
| Cluster membership | kine handles join/leave | etcd native discovery |

## 5. Implementation Plan

### Phase 1: Document and Validate ✅
- [x] Document current architecture (this document)
- [x] Verify no runtime SQLite/kine code paths exist
- [x] Confirm `--storage-backend=etcd3` is hard-coded

### Phase 2: Clean Up Vestigial SQLite
- [ ] **Remove SQLite CFLAGS from `build.zig`** — lines 101-102:
  ```
  -DSQLITE_ENABLE_DBSTAT_VTAB=1 -DSQLITE_USE_ALLOCA=1
  ```
- [ ] **Remove `go.etcd.io/bbolt`** from `go.mod` if not directly used by K8E code

### Phase 3: Harden Embedded etcd Configuration
- [ ] **Set explicit etcd config defaults**:
  - `MaxRequestBytes: 10485760` (10MB)
  - `MaxConcurrentStreams: 1000`
  - `QuotaBackendBytes: 2GB` (configurable via CLI)
  - `AutoCompactionMode: "revision"`
  - Snapshot cron via `--etcd-snapshot-schedule-cron`
- [ ] **Enable encryption at rest** — document AES-GCM workflow
- [ ] **Validate S3 backup integration**

### Phase 4: Testing
- [ ] Add regression tests for full lifecycle
- [ ] Performance benchmarks

## 6. Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| etcd corruption on unclean shutdown | Data loss | Enable auto-compaction; test recovery |
| Embedded etcd memory pressure | OOM | 2GB default quota; configurable |
| etcd version drift | Missed security fixes | Track k3s-io/etcd releases |
| No dev convenience fallback | Developer experience | Embedded etcd starts in <2s |

## 7. Future Work (Out of Scope)

- SQLite as optional backend for edge/IoT
- etcd migration tooling
- Multi-Raft / learner nodes