# KIP-26: Upgrade Dependencies to Kubernetes 1.37.0

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-09-11 | Implemented — tree tracks **v1.37.0-k3s1** (supersedes the [KIP-2](kip-2-upgrade-dependencies-to-kubernetes-1.35.md) baseline of **v1.35.5-k3s1**) |

## Summary

Move K8e's core dependency set from the Kubernetes `v1.35.5-k3s1` / etcd `v3.6.7` line to
Kubernetes `v1.37.0-k3s1` / etcd `v3.7.1`, tracking upstream k3s branch `release-1.37`. This
document captures the breaking changes encountered during the jump and how each was resolved.

## Motivation

K8e tracks upstream k3s, which tracks Kubernetes releases. KIP-2 landed the 1.35 baseline; this
KIP advances that baseline past two Kubernetes minor versions (1.36 and 1.37). The upgrade touches
the k8s staging modules, etcd v3.7, containerd v2.3, gRPC, helm-controller, and the rancher runtime
libraries — several of which changed exported APIs.

## Version matrix

### Toolchain and Kubernetes

| Component | Before | After |
|-----------|--------|-------|
| Go directive | `go 1.25.9` | `go 1.26.7` |
| `k8s.io/*` staging modules | `v0.35.1` / `v1.35.5-k3s1` | `v0.37.0` / `v1.37.0-k3s1` |
| `k8s.io/kubernetes` | `v1.35.5-k3s1` | `v1.37.0-k3s1` |
| `k8s.io/client-go` require | `v11.0.1-0.20190409021438-1a26190bd76a+incompatible` | `v0.37.0` |
| `k8s.io/kube-openapi` | `v0.0.0-20250910181357-589584f1c912` | `v0.0.0-20260721132016-d427ff9ee9ad` |
| `sigs.k8s.io/cri-tools` | `v1.35.0-k3s2` | `v1.37.0-k3s1` |

### Datastore, runtime, gRPC

| Component | Before | After |
|-----------|--------|-------|
| `go.etcd.io/etcd/*` | `v3.6.7-k3s1` | `v3.7.1-k3s1` |
| `github.com/containerd/containerd/v2` | `v2.2.3-k3s1` | `v2.3.4-k3s1` |
| `github.com/containerd/containerd/api` | `v1.10.0` | `v1.11.1` |
| `github.com/containerd/cgroups/v3` | `v3.1.2` | `v3.1.3` |
| `github.com/opencontainers/cgroups` | `v0.0.6` | `v0.0.7` |
| `github.com/opencontainers/selinux` | `v1.13.1` | `v1.15.1` |
| `github.com/Microsoft/hcsshim` | `v0.14.0-rc.1` | `v0.15.0-rc.1` |
| `github.com/Mirantis/cri-dockerd` | `v0.3.19-k3s3` | `v0.3.19-k3s5` |
| `google.golang.org/grpc` | `v1.72.2` | `v1.83.2` |
| `google.golang.org/protobuf` | `v1.36.11` | `v1.36.12` |

### Libraries

| Component | Before | After |
|-----------|--------|-------|
| `github.com/k3s-io/helm-controller` | `v0.16.5` | `v0.17.7` |
| `github.com/rancher/dynamiclistener` | `v0.6.0-rc1` | `v0.9.1-0.20260710234258-e4a1908ede0d` |
| `github.com/rancher/lasso` | `v0.2.6` | `v0.2.9` |
| `github.com/rancher/remotedialer` | `v0.4.1` | `v0.6.0-rc.1.0.20250916111157-f160aa32568d` |
| `github.com/rancher/wharfie` | `v0.6.7` | `v0.7.1` |
| `github.com/rancher/wrangler/v3` | `v3.4.0` | `v3.7.0` |
| `github.com/prometheus/client_golang` | `v1.23.2` | `v1.24.0` |
| `github.com/prometheus/common` | `v0.67.5` | `v0.70.0` |
| `github.com/emicklei/go-restful/v3` | `v3.12.2` | `v3.13.0` |
| `github.com/docker/docker` | `v28.3.3+incompatible` | `v28.5.2+incompatible` |
| `github.com/klauspost/compress` | `v1.18.5` | `v1.19.2` |
| `github.com/minio/minio-go/v7` | `v7.0.70` | `v7.1.0` |
| `github.com/otiai10/copy` | `v1.7.0` | `v1.14.1` |
| `github.com/rootless-containers/rootlesskit` | `v1.0.1` | `v1.1.1` |
| `github.com/sirupsen/logrus` | `v1.9.4` | `v1.10.2` |
| `github.com/stretchr/testify` | `v1.11.1` | `v1.12.1` |
| `go.uber.org/zap` | `v1.27.1` | `v1.28.0` |
| `golang.org/x/crypto` (replace pin) | `v0.45.0` | `v0.54.0` |
| `golang.org/x/net` (replace pin) | `v0.47.0` | `v0.57.0` |
| `golang.org/x/sys` (replace pin) | `v0.38.0` | `v0.47.0` |

## Module-level changes

### Added replacements

Kubernetes 1.37 promoted the kubelet CRI streaming server into a standalone staging module, so a
second one joined `k8s.io/externaljwt` (added by KIP-2):

```diff
 // go.mod replace block
+ k8s.io/cri-streaming => github.com/k3s-io/kubernetes/staging/src/k8s.io/cri-streaming v1.37.0-k3s1
+ k8s.io/streaming     => github.com/k3s-io/kubernetes/staging/src/k8s.io/streaming v1.37.0-k3s1
```

`go mod tidy` adds the matching `require ... v0.0.0 // indirect` lines.

### Dropped replacements

These pins were carrying dependencies that k8e no longer resolves; they are absent from k3s
`release-1.37` as well:

| Removed replace | Why it could go |
|-----------------|-----------------|
| `github.com/google/cadvisor => k3s-io/cadvisor v0.52.1` | no in-tree consumer in k8e |
| `github.com/open-policy-agent/opa => open-policy-agent/opa v0.59.0` | was only pinned to work around an hcsshim `v0.42.2` bad version; hcsshim is now `v0.15.0-rc.1` |
| `github.com/containerd/stargz-snapshotter => k3s-io/stargz-snapshotter v0.17.0-k3s1` | snapshotter now resolves through the containerd v2 graph |
| `github.com/opencontainers/cgroups => opencontainers/cgroups v0.0.4` | the `PidsLimit` `int64` → `*int64` break fixed in KIP-2 is gone; direct `require` bumped to `v0.0.7` |
| `gopkg.in/square/go-jose.v2 => gopkg.in/square/go-jose.v2 v2.6.0` | dependency no longer in the graph |

## Breaking changes and resolutions

### 1. helm-controller `pkg/crd` → `pkg/crds`, `List()` now returns an error

**Symptom:** `pkg/server/context.go` fails to build — the import path
`github.com/k3s-io/helm-controller/pkg/crd` no longer exists, and the helm-controller CRD listing
function no longer returns `[]crd.CRD`.

**Root Cause:** helm-controller v0.17.x renamed the package to `pkg/crds` and switched its CRD
listing to return plain `[]*apiextv1.CustomResourceDefinition` plus an `error`, instead of the
wrangler `crd.CRD` wrapper that k8e previously appended directly.

**Affected File:** `pkg/server/context.go`

**Resolution:** Rename the import, propagate the new error, and wrap each returned CRD in
`crd.CRD{Override: ...}` so the wrangler `BatchCreateCRDs` call site is unchanged:

```diff
- helmcrd "github.com/k3s-io/helm-controller/pkg/crd"
+ helmcrds "github.com/k3s-io/helm-controller/pkg/crds"
```

```diff
 func registerCrds(ctx context.Context, config *Config, restConfig *rest.Config) error {
 	factory, err := crd.NewFactoryFromClient(restConfig)
 	if err != nil {
 		return err
 	}

-	factory.BatchCreateCRDs(ctx, crds(config)...)
+	crdList, err := crds(config)
+	if err != nil {
+		return err
+	}
+	factory.BatchCreateCRDs(ctx, crdList...)

 	return factory.BatchWait()
 }

-func crds(config *Config) []crd.CRD {
+func crds(config *Config) ([]crd.CRD, error) {
 	defaultCrds := addoncrd.List()
 	if !config.ControlConfig.DisableHelmController {
-		defaultCrds = append(defaultCrds, helmcrd.List()...)
+		helmCRDs, err := helmcrds.List()
+		if err != nil {
+			return nil, errors.Wrap(err, "failed to list helm-controller CRDs")
+		}
+		for _, helmCRD := range helmCRDs {
+			defaultCrds = append(defaultCrds, crd.CRD{Override: helmCRD})
+		}
 	}
-	return defaultCrds
+	return defaultCrds, nil
 }
```

### 2. helm-controller `helmchart.Register` gained a `SecretCache` parameter

**Symptom:** `pkg/server/server.go` fails to build:

```
not enough arguments in call to helmchart.Register
```

**Root Cause:** helm-controller v0.17.x added a `SecretCache` argument (positioned after the
`core.V1().Secret()` clientset) so the chart controller can read chart secrets from an informer
cache instead of issuing a direct API call per reconcile.

**Affected File:** `pkg/server/server.go`

**Resolution:** Pass the generated Secret informer cache as the final argument:

```diff
 			auth.V1().ClusterRoleBinding(),
 			core.V1().ServiceAccount(),
 			core.V1().ConfigMap(),
-			core.V1().Secret())
+			core.V1().Secret(),
+			core.V1().Secret().Cache())
 	}
```

### 3. etcd v3.7 added `RangeStream` to `KVServer` (test mock)

**Symptom:** `go vet` / `go test` on `pkg/etcd` fails:

```
* mockEtcd does not implement etcdserverpb.KVServer (missing method RangeStream)
```

**Root Cause:** etcd v3.7 introduced a new gRPC-only `KV.RangeStream` server-streaming RPC
(chunked range responses, no REST gateway mapping). The generated `etcdserverpb.KVServer`
interface therefore gained a method, and the mock gRPC server used by the etcd unit tests no
longer satisfied it.

**Affected File:** `pkg/etcd/etcd_test.go`

**Resolution:** Implement the new RPC as an unsupported stub, consistent with the other
`mockEtcd` handlers:

```diff
 func (m *mockEtcd) Range(context.Context, *etcdserverpb.RangeRequest) (*etcdserverpb.RangeResponse, error) {
 	m.inc("range")
 	return nil, unsupported("range")
 }
+func (m *mockEtcd) RangeStream(*etcdserverpb.RangeRequest, grpc.ServerStreamingServer[etcdserverpb.RangeStreamResponse]) error {
+	m.inc("rangestream")
+	return unsupported("rangestream")
+}
```

### 4. gRPC codegen now requires embedded `Unimplemented*Server` structs

**Symptom:** After fixing (3), the same test file fails with:

```
* mockEtcd does not implement etcdserverpb.KVServer (missing method mustEmbedUnimplementedKVServer)
```

**Root Cause:** The `google.golang.org/grpc` bump to `v1.83.2` regenerated the etcd
`rpc_grpc.pb.go` service descriptors. With gRPC's default
`require_unimplemented_servers=true`, every service interface now carries an unexported
`mustEmbedUnimplemented<X>Server()` method, satisfied only by embedding the generated
`Unimplemented<X>Server` struct.

**Affected File:** `pkg/etcd/etcd_test.go`

**Resolution:** Embed the three generated unimplemented servers in `mockEtcd`. The mock's own
explicit methods continue to take precedence, so no behaviour changes for the RPCs the tests
actually exercise:

```diff
 type mockEtcd struct {
+	etcdserverpb.UnimplementedKVServer
+	etcdserverpb.UnimplementedClusterServer
+	etcdserverpb.UnimplementedMaintenanceServer
+
 	e           *ETCD
 	mu          *sync.RWMutex
 	calls       map[string]int
 	isLeader    bool
 	...
 }
```

### 5. In-tree build images were pinned to the old Go toolchain

**Symptom:** Building the `k8e-sandbox-cli` image fails before compiling anything:

```
go: go.mod requires go >= 1.26.7 (running go 1.25.9; GOTOOLCHAIN=local)
```

**Root Cause:** The `go 1.26.7` directive raises the minimum toolchain required by the module.
Official `golang` images ship with `GOTOOLCHAIN=local`, so a base image older than the directive
cannot build the module at all — it will not auto-download the newer toolchain the way a host
install with `GOTOOLCHAIN=auto` would.

**Affected Files:** `manifests/sandbox-mcp/Dockerfile`, `hack/test-sandbox-mcp-orbstack.sh`

**Resolution:** Bump the base images to `golang:1.26.7`. Note that CI needs no change — both
workflows use `actions/setup-go` with `go-version-file: go.mod`, and `hack/version.sh` derives
`VERSION_GOLANG` from the upstream k3s dependency manifest, so both track the new directive
automatically.

```diff
-FROM golang:1.25.9 AS build
+FROM golang:1.26.7 AS build
```

```diff
-image="${K8E_MCP_IMAGE:-golang:1.25.9}"
+image="${K8E_MCP_IMAGE:-golang:1.26.7}"
```

## Deliberate non-changes

These were audited and intentionally left alone:

- **`manifests/` image tags** (`metrics-server v0.8.1`, `coredns 1.10.1`,
  `local-path-provisioner v0.0.30`, …). k8e pins these independently of the Go module graph:
  k3s `release-1.35` **and** `release-1.37` both ship `metrics-server v0.9.0` / `coredns 1.14.7`,
  so the divergence predates this upgrade and is not caused by it. Changing them would be a
  separate, runtime-affecting decision.
- **`hack/version.sh` `VERSION_CNIPLUGINS`** stays at `v1.6.0-k3s1` while k3s uses
  `v1.9.1-k3s1`. Also pre-existing and orthogonal to the Go dependency graph.
- **k3s-only replacements** are intentionally absent, because k8e has no consumer for them:
  `cloudnativelabs/kube-router/v2`, `spegel-org/spegel`, `spf13/cobra`, `spf13/pflag`,
  `spf13/viper`. (k8e's `go.mod` replace block is otherwise a subset of k3s `release-1.37`.)
- **`k8s.io/controller-manager` / `k8s.io/mount-utils` appear as `v0.35.1 // indirect`**: this is
  exactly what `go mod tidy` computes, and upstream k3s `release-1.37` carries the same stale
  indirect versions (`v0.35.1` / `v0.35.0`). Both modules are overridden by `replace` to
  `v1.37.0-k3s1`, so the effective version is correct.

## Verification

Linux/arm64 (OrbStack container, `golang:1.25.9` bootstrap with `GOTOOLCHAIN=auto` resolving
`go1.26.7`):

```bash
# 1. module graph is stable
go mod tidy            # idempotent — no further changes

# 2. whole tree compiles
go build ./...

# 3. whole tree type-checks incl. every test file, with the official build tags
go vet -tags "ctrd netcgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp" ./...

# 4. the real release binary links (static)
go build -tags "ctrd netcgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp" \
  -buildvcs=false \
  -ldflags "-w -s -extldflags '-static -lm -ldl -lz -lpthread'" \
  -o /tmp/k8e ./cmd/server

# 5. affected packages' unit tests
go test -tags "ctrd netcgo osusergo providerless urfave_cli_no_docs apparmor seccomp" \
  -count=1 -timeout 300s ./pkg/etcd/... ./pkg/crd/... ./pkg/server/...
```

All of the above pass. Static linking requires `zlib1g-dev` and `libseccomp-dev` on the build host
(see KIP-2 §20).

## Reference

- Upstream k3s go.mod: `github.com/k3s-io/k3s` branch `release-1.37`
- helm-controller v0.17.7: `pkg/crds` package rename, `helmchart.Register` `SecretCache` parameter
- etcd v3.7.1: new gRPC-only `KV.RangeStream` RPC (`etcdserverpb/rpc_grpc.pb.go`)
- grpc-go codegen: `require_unimplemented_servers` and `mustEmbedUnimplemented<X>Server`
- Kubernetes v1.37: new staging modules `k8s.io/cri-streaming` and `k8s.io/streaming`
