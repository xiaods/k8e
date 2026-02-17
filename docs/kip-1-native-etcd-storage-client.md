# K8E Improvement Proposal: Native Embedded etcd Storage Client

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-02-17 | Implemented |

## Summary

Replace the `github.com/k3s-io/kine` storage abstraction layer with a native `clientv3`-based storage client that communicates directly with the embedded etcd server.

## Motivation

K8e inherited the kine dependency from K3s, where it serves as a multi-backend storage abstraction layer supporting SQLite, MySQL, PostgreSQL, NATS, and etcd. However, K8e is designed as an embedded-etcd-only distribution. In this context, kine introduces unnecessary complexity and indirection:

- The kine `client.Client` is a thin wrapper around `clientv3` — every method (`List`, `Get`, `Create`, `Update`, `Delete`) is a direct translation to etcd transactions and range queries.
- The `endpoint.Listen()` function, when the backend is etcd, simply returns the user-provided endpoints and TLS configuration without performing any meaningful work.
- The kine dependency pulls in drivers for SQLite, MySQL, PostgreSQL, and NATS — none of which are used by K8e.
- The `KineTLS` configuration flag and associated code paths add conditional logic that is irrelevant for a pure etcd backend.

By removing kine and implementing a lightweight native etcd client, K8e achieves a simpler, more maintainable storage layer with fewer transitive dependencies.

## Design

### Architecture Before

```
APIServer --> Runtime.EtcdConfig --> kine endpoint.Listen() --> Backend Driver
                                          |
                    +---------------------+---------------------+
                    |           |          |          |          |
                  SQLite     MySQL    PostgreSQL    NATS       etcd
```

### Architecture After

```
APIServer --> Runtime.EtcdConfig --> Embedded etcd (clientv3)
```

### New Package: `pkg/etcdstorage`

A new package `pkg/etcdstorage` provides a storage-oriented client interface backed by `go.etcd.io/etcd/client/v3`:

```go
type Client interface {
    List(ctx context.Context, key string, rev int) ([]Value, error)
    Get(ctx context.Context, key string) (Value, error)
    Create(ctx context.Context, key string, value []byte) error
    Update(ctx context.Context, key string, revision int64, value []byte) error
    Delete(ctx context.Context, key string, revision int64) error
    Close() error
}

type Value struct {
    Key      []byte
    Data     []byte
    Modified int64  // ModRevision for CAS operations
}
```

Each operation maps directly to etcd client primitives:

| Operation | etcd Implementation |
|-----------|-------------------|
| `List` | `clientv3.Get` with `WithPrefix()` and `WithRev()` |
| `Get` | `clientv3.Get` returning the first KV pair |
| `Create` | `clientv3.Txn` with `Compare(ModRevision(key), "=", 0).Then(OpPut)` |
| `Update` | `clientv3.Txn` with `Compare(ModRevision(key), "=", revision).Then(OpPut)` |
| `Delete` | `clientv3.Txn` with `Compare(ModRevision(key), "=", revision).Then(OpDelete)` |

The constructor `New(config.ETCDConfig) (Client, error)` creates a `clientv3.Client` with TLS configuration loaded from certificate file paths.

### New Configuration Types: `pkg/daemons/config`

Three local types replace the kine-imported types:

```go
// TLSConfig holds TLS certificate file paths for etcd connections.
type TLSConfig struct {
    CAFile   string
    CertFile string
    KeyFile  string
}

// DatastoreConfig holds configuration for the etcd datastore.
type DatastoreConfig struct {
    Endpoint         string
    BackendTLSConfig TLSConfig
    ServerTLSConfig  TLSConfig
    NotifyInterval   time.Duration
}

// ETCDConfig holds runtime etcd connection information.
type ETCDConfig struct {
    Endpoints   []string
    TLSConfig   TLSConfig
    LeaderElect bool
}
```

The field names (`CAFile`, `CertFile`, `KeyFile`) are deliberately identical to the kine `tls.Config` fields, ensuring that all existing code referencing `Datastore.BackendTLSConfig.CAFile` etc. continues to work without changes.

### Storage Initialization: `pkg/cluster/cluster.go`

The `startStorage()` function previously called `endpoint.Listen()` to initialize the kine layer. For embedded etcd, this function merely returned the configured endpoints and TLS settings. The replacement directly constructs the `ETCDConfig`:

```go
func (c *Cluster) startStorage(ctx context.Context, bootstrap bool) error {
    if c.storageStarted {
        return nil
    }
    c.storageStarted = true

    if !bootstrap {
        c.config.Datastore.ServerTLSConfig.CAFile = c.config.Runtime.ETCDServerCA
        c.config.Datastore.ServerTLSConfig.CertFile = c.config.Runtime.ServerETCDCert
        c.config.Datastore.ServerTLSConfig.KeyFile = c.config.Runtime.ServerETCDKey
    }

    endpoints := strings.Split(c.config.Datastore.Endpoint, ",")
    c.config.Runtime.EtcdConfig = config.ETCDConfig{
        Endpoints:   endpoints,
        TLSConfig:   c.config.Datastore.BackendTLSConfig,
        LeaderElect: true,
    }

    c.config.NoLeaderElect = false
    return nil
}
```

### Removed Components

| Component | Reason |
|-----------|--------|
| `migrateFromSQLite()` in `pkg/etcd/etcd.go` | SQLite backend no longer available without kine |
| `sqliteFile()` in `pkg/etcd/etcd.go` | Only used by `migrateFromSQLite()` |
| `KineTLS` field in `config.Control` | No kine socket to configure TLS for |
| `--kine-tls` CLI flag | Removed with `KineTLS` field |
| `EmulatedETCDVersion` in datastore config | Only used by kine to report version compatibility |

## Changes Summary

### New File

| File | Description |
|------|-------------|
| `pkg/etcdstorage/client.go` | Native etcd storage client using `clientv3` |

### Modified Files

| File | Changes |
|------|---------|
| `pkg/daemons/config/types.go` | Added `TLSConfig`, `DatastoreConfig`, `ETCDConfig` types; replaced `endpoint.Config` and `endpoint.ETCDConfig`; removed `KineTLS` field |
| `pkg/cluster/cluster.go` | Removed `endpoint.Listen()` call; direct etcd endpoint construction in `startStorage()` |
| `pkg/cluster/storage.go` | Replaced `kine/pkg/client` with `etcdstorage`; removed `KineTLS` conditional logic |
| `pkg/cluster/bootstrap.go` | Replaced `kine/pkg/client` and `kine/pkg/endpoint` with `etcdstorage` and `config.ETCDConfig` |
| `pkg/etcd/etcd.go` | Removed `migrateFromSQLite()`, `sqliteFile()`, and kine imports |
| `pkg/cli/server/server.go` | Removed `EmulatedETCDVersion` and `KineTLS` config assignments |
| `pkg/cli/cmds/server.go` | Removed `KineTLS` field and `--kine-tls` CLI flag |
| `go.mod` | Removed `github.com/k3s-io/kine v0.13.5` dependency |

### Impact: Net -79 lines of code (55 added, 134 removed)

## Compatibility

- **API Server**: No changes. The API server receives the same `--etcd-servers`, `--etcd-cafile`, `--etcd-certfile`, `--etcd-keyfile` flags through `setupStorageBackend()`, which reads from `Datastore.BackendTLSConfig` with identical field names.
- **Embedded etcd**: No changes. The embedded etcd server startup via `executor.ETCD()` is unaffected.
- **Bootstrap data**: The storage operations (`List`, `Create`, `Update`, `Delete`) have identical semantics and error formats, maintaining backward compatibility for encrypted bootstrap key management.
- **Cluster join/HA**: The `reconcileEtcd()` temporary etcd flow continues to work using `config.ETCDConfig` instead of `endpoint.ETCDConfig`.

## Breaking Changes

- **SQLite migration path removed**: Clusters that previously used SQLite (via kine) and were migrating to embedded etcd will no longer have an automatic migration. This path was inherited from K3s and is not part of K8e's supported upgrade path.
- **`--kine-tls` flag removed**: This hidden, experimental flag is no longer available.
- **External database backends removed**: MySQL, PostgreSQL, SQLite, and NATS are no longer supported as storage backends. K8e exclusively uses embedded etcd.

## Verification

1. **Build**: `go build ./...` passes with no compilation errors
2. **Dependency**: `grep -r "kine" --include="*.go" .` returns no import references
3. **go.mod**: `github.com/k3s-io/kine` is no longer listed as a direct dependency
4. **Tests**: `go test ./pkg/cluster/... ./pkg/etcd/... ./pkg/daemons/...`
