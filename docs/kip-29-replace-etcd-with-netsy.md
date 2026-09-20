# K8E Improvement Proposal: Replace etcd with Netsy

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-09-20 | Partially implemented (experimental, opt-in) — M1 single-node datastore shipped behind `--netsy`; multi-node HA still uses external `--datastore-endpoint` |

## Summary

Add [Netsy](https://netsy.dev) — a replicated key-value database that persists
to object storage and implements the subset of the etcd v3 gRPC API Kubernetes
uses (Range, Txn, Watch, MemberList, Status) — as a datastore k8e can run in
place of embedded etcd. A new `--netsy` flag on `k8e server` renders the Netsy
cluster config, generates the Netsy mTLS PKI, supervises the `netsy` process
and points kube-apiserver and the bootstrap storage client at it.

## Motivation

Embedded etcd keeps all cluster state on the node's disk, which ties the
control plane to a local volume and makes disaster recovery a snapshot/restore
exercise. Netsy writes the same key-value model to object storage (S3/GCS), so
the datastore survives the loss of the node and needs no local disk volume.

k8e is a good fit for this because the datastore abstraction is already
external-endpoint shaped:

- KIP-1 removed kine; `pkg/etcdstorage` talks to a real etcd v3 endpoint with
  `clientv3`.
- `--datastore-endpoint` plus `--datastore-cafile/-certfile/-keyfile` already
  select an external datastore, and `pkg/cluster` skips the managed/embedded
  driver whenever that endpoint is set.
- `pkg/daemons/control/server.go` already passes endpoint and TLS files to
  kube-apiserver through `setupStorageBackend`.

What was missing was the datastore *process* itself: Netsy has requirements
(TLS 1.3 with `netsy://` URI SANs, JSONC cluster config, several `NETSY_*`
environment variables, object storage credentials) that a bare
`--datastore-endpoint` cannot satisfy.

## Design

### Architecture before

```
k8e server --> embedded etcd (pkg/embedw, pkg/cluster managed driver)
                   |
                   +--> kube-apiserver (clientv3) --> local disk (WAL + snapshots)
```

### Architecture after (with --netsy)

```
k8e server
  |
  +--> pkg/netsy: render config => generate mTLS PKI => exec netsy => wait for a writable client API
  |         |
  |         +--> netsy process (:2378 client, :2381 peer, :8443 election, :8080 health)
  |                   |
  |                   +--> object storage (S3/GCS)
  |
  +--> Datastore.Endpoint = https://127.0.0.1:2378
  |    Datastore.BackendTLSConfig = generated CA + `client` role cert
  |
  +--> cluster.assignManagedDriver sees a non-empty endpoint => no embedded etcd
  |
  +--> kube-apiserver --etcd-servers=https://127.0.0.1:2378 (clientv3, mTLS)
```

### New package: `pkg/netsy`

Netsy's server code lives under `internal/`, so it cannot be imported as a Go
library. k8e runs the `netsy` binary and owns the three pieces of glue:

| File | Responsibility |
|------|----------------|
| `pkg/netsy/config.go` | `Config` validation, stable bind/advertise addresses, JSONC cluster config rendering, `NETSY_*` environment |
| `pkg/netsy/pki.go` | CA + peer server/client + k8e `client` certificates, idempotent reuse |
| `pkg/netsy/process.go` | `exec` supervision, bounded log tail, graceful stop |
| `pkg/netsy/client.go` | etcd v3 client TLS from the generated PKI and the readiness gate (client API reachable and writable) |

```go
cfg := netsy.Config{
    ClusterID: "k8e", NodeID: "k8e",
    DataDir: filepath.Join(serverDataDir, "netsy"),
    CertDir: filepath.Join(serverDataDir, "netsy", "tls"),
    ClientPort: 2378, PeerPort: 2381, ElectionPort: 8443, HealthPort: 8080,
    Storage: netsy.Storage{Provider: "s3", Bucket: "k8e-netsy"},
}
process, err := netsy.Start(ctx, cfg) // renders config, generates PKI, waits for a writable datastore
process.Endpoint()                     // https://127.0.0.1:2378
process.CertPaths()                    // CA + datastore client cert/key
```

### TLS identity

Netsy requires TLS 1.3 and identifies peers and clients by the URI SAN
`netsy://<cluster_id>/<role>/<name>` (role `peer` or `client`), with the
cluster id also present as the certificate Organization. `pkg/netsy` issues:

- a cluster CA (`O=<cluster_id>`),
- the Netsy node server certificate (`role=peer`, SANs for loopback, the
  advertise host and the node id),
- the Netsy local peer-client certificate (`role=peer`),
- the k8e datastore client certificate (`role=client`, name `k8e`) used by
  kube-apiserver and `pkg/etcdstorage`.

Certificates are reused when they still match the cluster, node and hosts, and
regenerated otherwise.

### Datastore wiring

`setupNetsy` in `pkg/cli/server/server.go` starts Netsy before
`server.StartServer` and sets:

```go
Datastore.Endpoint                       = process.Endpoint()
Datastore.BackendTLSConfig.CAFile        = certs.CA
Datastore.BackendTLSConfig.CertFile      = certs.DatastoreCert
Datastore.BackendTLSConfig.KeyFile       = certs.DatastoreKey
```

Everything downstream (apiserver flags, `pkg/etcdstorage` bootstrap) uses these
exactly as it does for `--datastore-endpoint`, and because the endpoint is set
`cluster.assignManagedDriver` does not fall back to the default embedded-etcd
driver, so no embedded etcd is started and `Runtime.ETCDReady` is closed
immediately. This assumes the data directory has never run embedded etcd:
`assignManagedDriver` prefers an initialized driver found on disk *before* it
looks at the endpoint, and that driver rewrites the endpoint back to its own
client URL (`pkg/etcd.ETCD.startClient`). A node whose data directory already
holds an embedded etcd datastore is therefore refused up front, rather than
running Netsy unused behind the embedded etcd (see Failure path).

The datastore a node runs on is recorded once Netsy became ready: the first
successful `--netsy` start writes a marker file
(`<data-dir>/server/db/netsy-backend`, and a failure to write it is logged), and
a later start *without* `--netsy` on that node is refused, so a flag dropped from
a service unit can never silently move the control plane back to embedded etcd
while the Kubernetes objects stay in the bucket. The Netsy process is supervised
for the whole server lifetime: if it dies after startup, `k8e server` exits
(naming the failed datastore) so the service manager restarts the control plane,
and a cancelled server context terminates the process with `SIGTERM` before the
`SIGKILL` fallback.

### Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--netsy` | `false` | Run Netsy as the datastore instead of embedded etcd |
| `--netsy-binary` | `netsy` | Path to the `netsy` executable |
| `--netsy-cluster-id` | `k8e` | Netsy cluster identifier (TLS organization) |
| `--netsy-node-id` | `k8e` | Identifier of this Netsy node |
| `--netsy-data-dir` | `<data-dir>/server/netsy` | Config, database and certificate directory |
| `--netsy-bucket` | — | Object storage bucket (required with `--netsy`) |
| `--netsy-key-prefix` | — | Object storage key prefix |
| `--netsy-storage-provider` | `s3` | `s3` or `gcs` |
| `--netsy-client-port` | `2378` | etcd-compatible client API |
| `--netsy-peer-port` | `2381` | Peer/replication API |
| `--netsy-election-port` | `8443` | Election health API |
| `--netsy-health-port` | `8080` | HTTP `/health` endpoint |

Object storage credentials come from the provider SDK environment
(`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`), which the Netsy child inherits.

`--netsy` is mutually exclusive with `--datastore-endpoint`, `--disable-etcd`
and `--disable-apiserver`, and is refused when the data directory already
contains an embedded etcd datastore; k8e fails fast with a clear error.

### Readiness and the write path

`netsy.Start` returns only once the client API answers an etcd v3 `Status` call
with a non-zero `Leader`, i.e. once the elector has promoted this node to
Primary. Netsy's HTTP `/health` endpoint turns healthy *before* that election
finishes and a write sent in that window fails with
`this node is not accepting writes (state: replica)`, so the health endpoint
alone is not a usable gate for pointing kube-apiserver at the datastore. The
probe uses the same mTLS identity (`role=client`) the datastore clients use.

Netsy implements the Kubernetes etcd subset, not the whole etcd v3 API: writes
must arrive as a `Txn` (one `MOD`+`EQUAL` compare, one put/delete on the same
key, and for updates/deletes the failure branch must be a single-key `Range`).
That is exactly how `k8s.io/apiserver/pkg/storage/etcd3` writes
(`OptimisticPut`/`OptimisticDelete` with `GetOnFailure: true`), so Kubernetes
objects work, while a bare `clientv3.KV.Put` or `DeleteRange` returns
`Unimplemented`.

## Failure path

If the `netsy` binary is missing, the bucket is unset, the object storage
credentials are wrong or the client API never becomes writable, `k8e server`
exits before the control plane starts with a message such as:

```
failed to start netsy datastore: timed out after 1m30s waiting for a writable netsy datastore at https://127.0.0.1:2378
<last netsy log lines>
```

The repair is to check `--netsy-binary`, `--netsy-bucket` and the provider
credentials, then restart. A datastore that dies *after* startup is reported the
same way instead of leaving the control plane serving without a datastore:

```
Netsy datastore exited unexpectedly (error: exit status 1):
<last netsy log lines>
```

A node that already ran Netsy also refuses to start without `--netsy`, so a flag
dropped by mistake names the datastore it would silently switch away from
instead of starting a second, empty one:

```
invalid flag use; /var/lib/k8e/server records that this node's Kubernetes data is stored in Netsy, so starting embedded etcd would silently reopen a different datastore. Keep running with --netsy (or point --datastore-endpoint at the datastore holding the data); after migrating the data back, remove /var/lib/k8e/server/db/netsy-backend
```

On a node that never ran Netsy, k8e behaves exactly as before when the flag is
absent.

A server that already ran embedded etcd fails before the Netsy process is
started, because that embedded datastore would win over `--netsy`:

```
invalid flag use; /var/lib/k8e/server already holds an embedded etcd datastore (/var/lib/k8e/server/db/etcd/member/wal); --netsy would run Netsy but the control plane would stay on that embedded etcd. Point --data-dir at an empty directory to store the Kubernetes data in Netsy, or keep running embedded etcd
```

## Milestones

| Milestone | Scope | Status |
|-----------|-------|--------|
| M1 | Single-node Netsy datastore: config, PKI, supervision, datastore wiring, opt-in real-netsy test | Shipped in this KIP |
| M2 | Multi-node Netsy HA (per-node certificates, cluster membership, peer ports) | Not started — deployments can already run Netsy out of band and use `--datastore-endpoint` |
| M3 | Remove the embedded-etcd backend entirely once Netsy is the default | Not started |
| M4 | Snapshot/restore against object storage (`k8e etcd-snapshot` equivalents) | Not started |

## Risks and limitations

- Netsy's `LeaseGrant`/`LeaseKeepAlive` RPCs are stubs. Only the
  `coordination.k8s.io/v1` `Lease` *objects* (node heartbeats) are ordinary keys
  and therefore unaffected: the etcd3 storage backend in `k8s.io/apiserver`
  grants an etcd lease for every object written with a TTL, and the Events
  registry does exactly that (`--event-ttl`, 1h by default). Event expiry on
  Netsy is therefore **unverified** — the stub lease must be replaced upstream
  and checked with an Events create → expire → restart run against a real
  `kube-apiserver` before this backend is used for a cluster whose events
  matter.
- The single-node default disables quorum replication (`replication.quorum: 0`),
  so durability depends on the object storage provider. Multi-node HA is M2.
- Only the etcd operations the Kubernetes data path uses are served: writes go
  through `Txn` (`clientv3.KV.Put`/`DeleteRange` are `Unimplemented`), so a
  datastore consumer that bypasses `k8s.io/apiserver/pkg/storage/etcd3` —
  including `kubectl`-style direct etcd clients — will not work.
- k8e does not vendor the `netsy` binary; operators install it or pass
  `--netsy-binary`. The config template, certificate layout and readiness probe
  in `pkg/netsy` were written and validated against `netsy-dev/netsy` commit
  [`f1697fe`](https://github.com/netsy-dev/netsy/commit/f1697fe75331dbbed7313dcef25cf80068d46dcb),
  so pin that build (or a reviewed successor) per node: another build may render
  an incompatible `netsy.jsonc` or expect a different certificate layout.

## Verification

- `pkg/netsy` unit tests cover config validation/rendering, PKI roles, SANs,
  reuse and the renewal window (an expiring or corrupted chain is reissued),
  readiness timeouts, early exit, mTLS client construction, shutdown, the
  supervision contract (`SIGTERM` on context cancel, a crash distinguishable
  from a requested stop) and bounded log capture.
- `pkg/cli/server` unit tests pin the two guard rails: `--netsy` is refused on a
  node that holds embedded etcd data, and a node marked as running Netsy refuses
  to start without the flag.
- `TestIntegrationRealNetsyDatastore` (opt-in, `NETSY_BINARY` + `NETSY_DEV_S3`)
  starts a real `netsy` against a fake S3 server, generates the PKI, waits for
  the write-ready client API and drives the Kubernetes write path with
  `go.etcd.io/etcd/client/v3/kubernetes` — the exact wrapper `kube-apiserver`'s
  etcd3 store uses — plus prefix `List`/`Count` and `Watch` over the same mTLS
  endpoint `pkg/etcdstorage` connects to.

## Rollback

`--netsy` is off by default and changes no existing code path. The two rollbacks
below need nothing but the flag:

- A node that already holds embedded etcd data is refused `--netsy` before
  anything is started, so its datastore stays where it is.
- A node whose data must not move back keeps its embedded etcd as long as the
  flag stays off — and the marker refuses the switch if it is dropped.

Switching a node *back* from Netsy is a deliberate step, because the Kubernetes
objects live in the bucket while the embedded etcd directory still holds whatever
it held before the switch. Either point `--datastore-endpoint` at that Netsy
client API (the supported cluster datastore path, which reads the same data), or
delete `<data-dir>/server/db/netsy-backend` and accept embedded etcd again: a
fresh datastore if `<data-dir>/server/db/etcd` is empty, otherwise the data that
was left there when the node moved to Netsy.
