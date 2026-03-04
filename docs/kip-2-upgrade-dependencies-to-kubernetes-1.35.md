# KIP-2: Upgrade Dependencies to Kubernetes 1.35.1

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-03-04 | Done |

## Summary

Upgrade K8e's core dependencies to align with Kubernetes v1.35.1 and etcd v3.6.7, tracking the upstream k3s release-1.35 branch. This document captures the breaking changes encountered during the upgrade and their resolutions.

## Motivation

K8e tracks upstream k3s, which tracks Kubernetes releases. The upgrade from the previous dependency set to Kubernetes v1.35.1 + etcd v3.6.7 involves multiple breaking changes in transitive dependencies that require coordinated fixes in `go.mod` and source code.

## Breaking Changes and Resolutions

### 1. etcd `client/v2` Module Removed in etcd v3.6.x

**Error:**
```
go: go.etcd.io/etcd/client/v2@v2.306.7: missing github.com/k3s-io/etcd/client/go.mod
and .../v2/go.mod at revision client/v2.306.7-k3s1
```

**Root Cause:** etcd v3.6.x removed the `client/v2` sub-module entirely. The `k3s-io/etcd` fork has a `client/v2.306.7-k3s1` tag, but the tag points to a tree with no `client/v2/go.mod` file. No etcd v3.6.x sub-module depends on `client/v2` anymore.

**Resolution:** Remove both the replace directive and the indirect require:

```diff
 // go.mod replace block
- go.etcd.io/etcd/client/v2 => github.com/k3s-io/etcd/client/v2 v2.306.7-k3s1

 // go.mod require block
- go.etcd.io/etcd/client/v2 v2.306.7 // indirect
```

### 2. etcd `raft/v3` Moved to Separate Repository

**Error:**
```
go: go.etcd.io/etcd/raft/v3@v3.6.7: reading go.etcd.io/etcd/raft/go.mod
at revision raft/v3.6.7: unknown revision raft/v3.6.7
```

**Root Cause:** Starting with etcd v3.6.x, the raft module was extracted into a standalone repository at `github.com/etcd-io/raft` with the module path `go.etcd.io/raft/v3`. The old path `go.etcd.io/etcd/raft/v3` no longer has v3.6.x releases. The etcd server's `go.mod` now depends on `go.etcd.io/raft/v3 v3.6.0`.

**Resolution:** Remove the stale indirect require. `go mod tidy` will pull in the correct `go.etcd.io/raft/v3` automatically:

```diff
 // go.mod require block
- go.etcd.io/etcd/raft/v3 v3.6.7 // indirect
```

### 3. runc v1.4.0 Removed `libcontainer/userns`

**Error:**
```
go: github.com/xiaods/k8e/pkg/agent/containerd imports
    github.com/opencontainers/runc/libcontainer/userns: module
    github.com/opencontainers/runc@v1.4.0 does not contain package
    github.com/opencontainers/runc/libcontainer/userns
```

**Root Cause:** runc v1.4.0 removed the deprecated `libcontainer/userns` package. The functionality was extracted to `github.com/moby/sys/userns` (the replacement announced since runc v1.2.x).

**Affected Files:**
- `pkg/agent/containerd/config_linux.go`
- `pkg/daemons/agent/agent_linux.go`

**Resolution:** Replace the import. The API is identical (`userns.RunningInUserNS()` signature unchanged):

```diff
- "github.com/opencontainers/runc/libcontainer/userns"
+ "github.com/moby/sys/userns"
```

### 4. runc v1.4.0 Removed `libcontainer/cgroups`

**Error:**
```
go: github.com/xiaods/k8e/pkg/rootless imports
    github.com/opencontainers/runc/libcontainer/cgroups: module
    github.com/opencontainers/runc@v1.4.0 does not contain package
    github.com/opencontainers/runc/libcontainer/cgroups
```

**Root Cause:** runc v1.3.0+ extracted `libcontainer/cgroups` into `github.com/opencontainers/cgroups`. The `ParseCgroupFile` function signature is unchanged.

**Affected File:**
- `pkg/rootless/rootless.go`

**Resolution:**

```diff
- "github.com/opencontainers/runc/libcontainer/cgroups"
+ "github.com/opencontainers/cgroups"
```

### 5. New Staging Module `k8s.io/externaljwt`

**Error:**
```
go: k8s.io/kubernetes/pkg/serviceaccount/externaljwt/plugin imports
    k8s.io/externaljwt/apis/v1: reading k8s.io/externaljwt/go.mod
    at revision v0.0.0: unknown revision v0.0.0
```

**Root Cause:** Kubernetes v1.35 introduced a new staging module `k8s.io/externaljwt` for external JWT signer support. Like all `k8s.io/*` staging modules, it requires a replace directive pointing to the k3s fork's staging directory.

**Resolution:** Add both a replace directive and an indirect require, following the pattern of all other `k8s.io/*` staging modules:

```diff
 // go.mod replace block
+ k8s.io/externaljwt => github.com/k3s-io/kubernetes/staging/src/k8s.io/externaljwt v1.35.1-k3s1

 // go.mod require block
+ k8s.io/externaljwt v0.0.0 // indirect
```

### 6. opencontainers/runtime-spec v1.3.0 Changed `LinuxPids.Limit` to `*int64`

**Error:**
```
cannot use *resourceConfig.PidsLimit (variable of type int64) as *int64 value in assignment
```

**Root Cause:** `opencontainers/runtime-spec` v1.3.0 changed `LinuxPids.Limit` from `int64` to `*int64`. This causes type mismatches in containerd, cgroups, and kubernetes packages.

**Resolution:** Pin runtime-spec to v1.2.1 via replace (same as k3s release-1.35):

```diff
 // go.mod replace block
+ github.com/opencontainers/runtime-spec => github.com/opencontainers/runtime-spec v1.2.1
```

### 7. opencontainers/cgroups v0.0.6 Changed `PidsLimit` to `*int64`

**Error:**
```
cannot use *resourceConfig.PidsLimit (variable of type int64) as *int64 value in assignment
```

**Root Cause:** `opencontainers/cgroups` v0.0.6 changed `Resources.PidsLimit` from `int64` to `*int64`. k3s release-1.35 uses v0.0.4 which has the original `int64` type.

**Resolution:** Pin opencontainers/cgroups to v0.0.4 via replace:

```diff
 // go.mod replace block
+ github.com/opencontainers/cgroups => github.com/opencontainers/cgroups v0.0.4
```

### 8. containerd/cgroups v1/v2 → v3 Migration

**Error:**
```
undefined: cgroupsv2.LoadManager
```

**Root Cause:** k3s release-1.35 migrated from `containerd/cgroups` (v1) and `containerd/cgroups/v2` to `containerd/cgroups/v3`. The API changed: `LoadManager(mountpoint, group)` → `Load(group)` (default mountpoint is `/sys/fs/cgroup`).

**Affected File:** `pkg/cgroups/cgroups_linux.go`

**Resolution:**

```diff
- "github.com/containerd/cgroups"
- cgroupsv2 "github.com/containerd/cgroups/v2"
+ cgroups "github.com/containerd/cgroups/v3"
+ cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2"

- cgroupsv2.LoadManager("/sys/fs/cgroup", "/")
+ cgroupsv2.Load("/")
```

Also remove `github.com/containerd/cgroups v1.1.0` from direct requires.

### 9. containerd v1 cmd/ctr → containerd/v2 Migration

**Error:**
```
linux.Marshal undefined (type *"github.com/containerd/cgroups/v3/cgroup1/stats".Metrics
has no field or method Marshal)
```

**Root Cause:** k8e's `pkg/ctr/main.go` imported `github.com/containerd/containerd/cmd/ctr/app` (containerd v1), which transitively imports `cmd/ctr/commands/tasks/metrics.go`. That file calls `linux.Marshal()` on a cgroups stats type. With the hcsshim v0.13.0 replace (needed by containerd/v2), the stats type uses `google.golang.org/protobuf` which doesn't generate `Marshal()` methods (unlike the old `gogo/protobuf`).

k3s avoids this entirely because they migrated to containerd/v2 for the ctr CLI.

**Affected File:** `pkg/ctr/main.go`

**Resolution:** Migrate to containerd v2's ctr (matches k3s release-1.35). Note the `urfave/cli` v1 → v2 API change:

```diff
- "github.com/containerd/containerd/cmd/ctr/app"
- "github.com/containerd/containerd/pkg/seed"
- "github.com/urfave/cli"
+ "github.com/containerd/containerd/v2/cmd/ctr/app"
+ "github.com/urfave/cli/v2"

- seed.WithTimeAndRand()
  app := app.New()
  for i, flag := range app.Flags {
-   if sFlag, ok := flag.(cli.StringFlag); ok {
-     if sFlag.Name == "address, a" {
+   if sFlag, ok := flag.(*cli.StringFlag); ok {
+     if sFlag.Name == "address" {
```

### 10. spegel Bootstrapper Interface Changed

**Error:**
```
*selfBootstrapper does not implement routing.Bootstrapper (wrong type for method Get)
    have Get() (*peer.AddrInfo, error)
    want Get(context.Context) ([]peer.AddrInfo, error)
```

**Root Cause:** The spegel dependency (`github.com/k3s-io/spegel v0.6.0-k3s1`) changed the `Bootstrapper` interface:
- `Get()` → `Get(ctx context.Context) ([]peer.AddrInfo, error)` (added context, returns slice)
- `Run(ctx, string)` → `Run(ctx, peer.AddrInfo) error` (changed id from string to AddrInfo)
- `NewP2PRouter` options changed from `libp2p.Option` to `routing.P2PRouterOption`
- `router.Ready()` → `router.Ready(ctx)` (added context)
- `oci.NewContainerd` API completely changed, split into `oci.NewClient` + `oci.NewContainerd` (Store)
- `registry.NewRegistry` now returns `(*Registry, error)`, uses `reg.Handler()` instead of `reg.Server()`

**Affected Files:**
- `pkg/spegel/bootstrap.go` — Complete rewrite of all bootstrapper types
- `pkg/spegel/spegel.go` — Updated all API calls
- `pkg/spegel/store.go` — New file: deferred OCI store (ported from k3s)

**Resolution:** Port from k3s release-1.35's `pkg/spegel/` package, adapting imports from `k3s-io/k3s` to `xiaods/k8e`. Key additions:
- `notSelfBootstrapper` wrapper (prevents router considering itself as only peer)
- `waitForDone` helper (blocks Run until context is done)
- `DeferredStore` for lazy containerd connection

### 11. Kubernetes 1.35 `NewSchedulerCommand` Signature Change

**Error:**
```
not enough arguments in call to sapp.NewSchedulerCommand
    have ()
    want (<-chan struct{}, ...Option)
```

**Root Cause:** Kubernetes v1.35 added a `stopCh <-chan struct{}` parameter to `NewSchedulerCommand`.

**Affected File:** `pkg/daemons/executor/embed.go`

**Resolution:**

```diff
- command := sapp.NewSchedulerCommand()
+ command := sapp.NewSchedulerCommand(ctx.Done())
```

### 12. etcd v3.6.7 API Changes (3 issues)

#### 12a. `snapshotv3.Save` → `snapshotv3.SaveWithVersion`

**Error:** `undefined: snapshotv3.Save`

**Root Cause:** etcd v3.6.7 renamed `Save` to `SaveWithVersion` and it now returns `(version string, err error)`.

**Affected File:** `pkg/etcd/snapshot.go`

```diff
- if err := snapshotv3.Save(ctx, e.client.GetLogger(), *cfg, snapshotPath); err != nil {
+ if _, err := snapshotv3.SaveWithVersion(ctx, e.client.GetLogger(), *cfg, snapshotPath); err != nil {
```

#### 12b. `etcdserver.ErrNoLeader` Moved to Errors Subpackage

**Error:** `undefined: etcdserver.ErrNoLeader`

**Root Cause:** etcd v3.6.7 moved `ErrNoLeader` from `etcdserver` to `etcdserver/errors`.

**Affected File:** `pkg/etcd/etcd.go`

```diff
- "go.etcd.io/etcd/server/v3/etcdserver"
+ etcderrors "go.etcd.io/etcd/server/v3/etcdserver/errors"

- return etcdserver.ErrNoLeader
+ return etcderrors.ErrNoLeader
```

#### 12c. `credentials.NewBundle` → `credentials.NewTransportCredential`

**Error:** `undefined: credentials.NewBundle` / `undefined: credentials.Config`

**Root Cause:** etcd v3.6.7 replaced the `NewBundle(Config{})` pattern with `NewTransportCredential()`.

**Affected File:** `pkg/etcd/etcd.go`

```diff
- creds := credentials.NewBundle(credentials.Config{TLSConfig: cfg.TLS}).TransportCredentials()
+ creds := credentials.NewTransportCredential(cfg.TLS)
```

### 13. etcd rpctypes Typo Fix

**Error:** `undefined: rpctypes.ErrGPRCNotSupportedForLearner`

**Root Cause:** etcd v3.6.7 fixed a typo in the constant name: `GPRC` → `GRPC`.

**Affected File:** `pkg/cluster/storage.go`

```diff
- rpctypes.ErrGPRCNotSupportedForLearner
+ rpctypes.ErrGRPCNotSupportedForLearner
```

### 14. Go 1.25 Stricter Vet: Non-constant Format Strings

**Error:**
```
pkg/node/controller.go:91:15: non-constant format string in call to (*github.com/sirupsen/logrus.Logger).Errorf
```

**Root Cause:** Go 1.25 (used by Kubernetes 1.35) introduced stricter vet checks that reject string concatenation as the format argument to printf-like functions (`logrus.Errorf`, `logrus.Warnf`, `logrus.Infof`, `t.Errorf`). This prevents accidental format string injection.

**Affected Files:**
- `pkg/node/controller.go`
- `pkg/clientaccess/token.go`
- `pkg/cluster/bootstrap.go`
- `pkg/util/gates_test.go`

**Resolution:** Convert string concatenation to proper format strings:

```diff
 // pkg/node/controller.go
- logrus.Errorf("No InternalIP addresses found for node " + nodeName)
+ logrus.Errorf("No InternalIP addresses found for node %s", nodeName)

 // pkg/clientaccess/token.go
- logrus.Warnf(warning.Text)
+ logrus.Warnf("%s", warning.Text)

 // pkg/cluster/bootstrap.go
- logrus.Infof("Cluster reset: backing up certificates directory to " + tlsBackupDir)
+ logrus.Infof("Cluster reset: backing up certificates directory to %s", tlsBackupDir)

 // pkg/util/gates_test.go
- t.Errorf("error, should be " + tt.want + ", but got " + got)
+ t.Errorf("error, should be %s, but got %s", tt.want, got)
```

### 15. etcd v3.6.7 `ErrNotEnoughStartedMembers` Moved (Test File)

**Error:** `undefined: etcdserver.ErrNotEnoughStartedMembers`

**Root Cause:** Same as §12b — etcd v3.6.7 moved error constants from `etcdserver` to `etcdserver/errors`. The test file `etcd_test.go` also referenced these moved constants.

**Affected File:** `pkg/etcd/etcd_test.go`

**Resolution:**

```diff
- "go.etcd.io/etcd/server/v3/etcdserver"
+ etcderrors "go.etcd.io/etcd/server/v3/etcdserver/errors"

- etcdserver.ErrNotEnoughStartedMembers
+ etcderrors.ErrNotEnoughStartedMembers

- etcdserver.ErrNoLeader
+ etcderrors.ErrNoLeader
```

### 16. grpc `manual.Resolver.CC` Changed from Field to Method

**Error:**
```
pkg/etcd/resolver.go:33:5: comparison of function CC != nil is always true
```

**Root Cause:** In newer versions of `google.golang.org/grpc/resolver/manual`, `CC` was changed from a struct field to a method. Comparing a method to `nil` is always true (the method value is never nil on a non-nil receiver). Additionally, after a successful `Build()` call, the client connection is always set.

**Affected File:** `pkg/etcd/resolver.go`

**Resolution:** Remove the redundant nil check:

```diff
- if r.CC != nil {
-     addr, serverName := interpret(r.endpoint)
-     r.UpdateState(resolver.State{
-         Addresses: []resolver.Address{{Addr: addr, ServerName: serverName}},
-     })
- }
+ addr, serverName := interpret(r.endpoint)
+ r.UpdateState(resolver.State{
+     Addresses: []resolver.Address{{Addr: addr, ServerName: serverName}},
+ })
```

### 17. `gspt` Requires CGO_ENABLED=1

**Error:**
```
github.com/erikdubbelboer/gspt: build constraints exclude all Go files
```

**Root Cause:** `github.com/erikdubbelboer/gspt` (used for setting process titles via `prctl`) requires cgo on Linux. When `CGO_ENABLED=0`, all Go files in the package are excluded by build constraints. Multiple `pkg/cli/*` packages import `gspt` either directly or via `pkg/proctitle`, causing cascading build failures across `pkg/cli/agent`, `pkg/cli/server`, `pkg/cli/cert`, `pkg/cli/token`, `pkg/cli/etcdsnapshot`, `pkg/cli/secretsencrypt`, and all `cmd/*` packages.

**Affected Files (direct `gspt` imports):**
- `pkg/cli/agent/agent.go`
- `pkg/cli/token/token.go`
- `pkg/proctitle/proctitle.go`

**Resolution:** Run tests with `CGO_ENABLED=1` (or use `make test`):

```bash
CGO_ENABLED=1 go test ./...
```

### 18. Containerd v1 → v2 Full Migration

**Root Cause:** k8e was using `github.com/containerd/containerd` (v1.7.23) which is incompatible with Kubernetes 1.35's CRI API. k3s release-1.35 has fully migrated to `github.com/containerd/containerd/v2`. The v1 root package also caused protobuf namespace conflicts (see Protobuf section above).

**Key Changes:**

All containerd imports migrated from v1 to v2 paths:
- `github.com/containerd/containerd` → `github.com/containerd/containerd/v2/client`
- `github.com/containerd/containerd/images` → `github.com/containerd/containerd/v2/core/images`
- `github.com/containerd/containerd/namespaces` → `github.com/containerd/containerd/v2/pkg/namespaces`
- `github.com/containerd/containerd/errdefs` → `github.com/containerd/errdefs` (extracted to separate module)
- `github.com/containerd/containerd/pkg/cri/labels` → local constants (made internal in v2)
- `github.com/containerd/containerd/pkg/cri/constants` → local constants (made internal in v2)
- `github.com/containerd/containerd/cmd/ctr/app` → `github.com/containerd/containerd/v2/cmd/ctr/app`
- `github.com/containerd/containerd/pkg/seed` → removed (no longer exists in v2)

CRI labels/constants that were previously imported from containerd packages are now defined locally:

```go
const (
    criContainerdPrefix       = "io.cri-containerd"
    criPinnedImageLabelKey    = criContainerdPrefix + ".pinned"
    criPinnedImageLabelValue  = "pinned"
    criK8sContainerdNamespace = "k8s.io"
)
```

**Affected Files (9 files):**

| File | Changes |
|------|---------|
| `pkg/agent/containerd/containerd.go` | v2 client/images/namespaces imports, local CRI constants, `errdefs` extracted |
| `pkg/agent/containerd/config_linux.go` | v2 client, `overlayutils` path, `fuse-overlayfs-snapshotter/v2` |
| `pkg/agent/containerd/config_windows.go` | v2 client |
| `pkg/containerd/builtins.go` | All plugin imports → v2 paths, added new v2 plugins |
| `pkg/containerd/builtins_linux.go` | Removed aufs, added erofs/blockfile plugins, `fuse-overlayfs/v2`, `zfs/v2` |
| `pkg/containerd/builtins_windows.go` | v2 paths for diff/snapshots |
| `pkg/containerd/builtins_cri.go` | Split CRI import into 3: `plugins/cri`, `plugins/cri/images`, `plugins/cri/runtime` |
| `pkg/containerd/main.go` | v2 command path, removed `seed.WithTimeAndRand()` |
| `pkg/containerd/utility_linux.go` | v2 paths for overlayutils, `fuse-overlayfs/v2` |

**go.mod Changes:**

```diff
- github.com/containerd/containerd v1.7.23
+ github.com/containerd/containerd/v2 v2.1.5-k3s1
- github.com/containerd/aufs v1.0.0
+ github.com/containerd/errdefs v1.0.0
- github.com/containerd/fuse-overlayfs-snapshotter v1.0.8
+ github.com/containerd/fuse-overlayfs-snapshotter/v2 v2.1.6
- github.com/containerd/zfs v1.1.0
+ github.com/containerd/zfs/v2 v2.0.0-rc.0
```

### 19. Build System Aligned with k3s Upstream (System GCC, No Zig CC)

**Root Cause:** The original `build.zig` used zig cc as the C compiler for cross-compilation with musl libc. This caused a critical issue: zig cc's musl `audit.h` header defines `AUDIT_ARCH_M68K`, `AUDIT_ARCH_SH`, and `AUDIT_ARCH_SHEB` all as `0xFFFFFFFF` (unsupported), which causes duplicate switch case errors in `libseccomp-golang` under Go 1.22+.

After investigating multiple workarounds (CGO_CFLAGS defines, `-include` header, sed patching), we aligned with k3s's upstream `scripts/build` which uses **system gcc for all components** — no zig cc at all.

**Changes to `build.zig`:**

| Component | Before | After (aligned with k3s) |
|-----------|--------|--------------------------|
| **k8e binary** | zig cc as CC/CXX, musl target | System gcc, `CGO_CFLAGS="-DSQLITE_ENABLE_DBSTAT_VTAB=1 -DSQLITE_USE_ALLOCA=1"` |
| **containerd-shim** | zig cc, GOOS/GOARCH env vars | System gcc, GOPATH-based, tags with `netgo` (not `netcgo`) |
| **runc** | zig cc with musl (broken) | System gcc, `make EXTRA_LDFLAGS="-w -s" BUILDTAGS="apparmor seccomp" static` |
| **CNI plugins** | GOOS/GOARCH env vars | Native build only (removed cross-compile env) |
| **Tags** | Dynamic (`-Dseccomp`, `-Dselinux`, `-Dapparmor`) | Hardcoded: `ctrd netcgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp` |
| **Static ldflags** | `-extldflags '-static'` | `-extldflags '-static -lm -ldl -lz -lpthread'` |
| **PKG_CONTAINERD** | `github.com/containerd/containerd` | `github.com/containerd/containerd/v2` |

Removed from `build.zig`:
- Build options: `use_seccomp`, `use_selinux`, `use_apparmor`
- Helper function: `buildGoBinary` (was zig cc wrapper)
- Helper function: `goarch_str` (no longer needed without cross-compilation)
- Clean target: `.libseccomp` directory

### 20. Static Linking Build Dependencies

Static linking with system gcc requires platform-specific static library packages. Without them, the linker fails with `cannot find -lresolv`, `-lm`, `-lz`, `-lc`, `-lseccomp` errors.

**Required packages by platform:**

| Library | Ubuntu (apt) | Amazon Linux (yum) | Purpose |
|---------|-------------|-------------------|---------|
| libc.a, libm.a, libdl.a, libpthread.a, libresolv.a | `libc6-dev` | `glibc-static` | glibc static libraries |
| libz.a | `zlib1g-dev` | `zlib-static` | zlib compression |
| libseccomp.a + headers | `libseccomp-dev` | `libseccomp-static` + `libseccomp-devel` | seccomp syscall filtering |

**Install commands:**

```bash
# Ubuntu
sudo apt-get update && sudo apt-get install -y libc6-dev zlib1g-dev libseccomp-dev

# Amazon Linux
sudo yum install -y glibc-static zlib-static libseccomp-static libseccomp-devel
```

**Note:** k3s builds inside a Docker container (via `Dockerfile.dapper`) that has all static libraries pre-installed. When building k8e directly on the host, these packages must be installed manually.

### 21. EgressSelectorConfiguration Strict Decoding Failure

**Error:**
```
Error: failed to read egress selector config: strict decoding error: unknown field "EgressSelections"
```

**Root Cause:** `genEgressSelectorConfig()` in `pkg/daemons/control/deps/deps.go` used the internal (unversioned) type `k8s.io/apiserver/pkg/apis/apiserver.EgressSelectorConfiguration` to construct the egress selector config, then serialized it with `json.Marshal`. Internal types have no JSON struct tags, so the `EgressSelections` field serialized as `"EgressSelections"` (PascalCase). However, the apiserver reads this config file using strict decoding against the `v1beta1` schema, which expects `"egressSelections"` (camelCase, from JSON tag). The field name mismatch caused a strict decoding error.

**Affected File:** `pkg/daemons/control/deps/deps.go`

**Resolution:** Replace all internal type references with the versioned `v1beta1` types that have correct JSON tags:

```diff
- "k8s.io/apiserver/pkg/apis/apiserver"
+ apiserverv1beta1 "k8s.io/apiserver/pkg/apis/apiserver/v1beta1"

- var clusterConn apiserver.Connection
+ var clusterConn apiserverv1beta1.Connection

- egressConfig := apiserver.EgressSelectorConfiguration{
+ egressConfig := apiserverv1beta1.EgressSelectorConfiguration{

- EgressSelections: []apiserver.EgressSelection{
+ EgressSelections: []apiserverv1beta1.EgressSelection{
```

### 22. Kubelet `--pod-infra-container-image` Flag Removed in Kubernetes 1.35

**Error:**
```
Error: failed to parse kubelet flag: unknown flag: --pod-infra-container-image
```

**Root Cause:** The `--pod-infra-container-image` kubelet flag was deprecated and fully removed in Kubernetes 1.35. The pause container image is now managed entirely by the container runtime (containerd/CRI), not kubelet.

**Affected Files:**
- `pkg/daemons/agent/agent_linux.go`
- `pkg/daemons/agent/agent_windows.go`

**Resolution:** Remove the `pod-infra-container-image` argument from kubelet args on both platforms:

```diff
- if cfg.PauseImage != "" {
-     argsMap["pod-infra-container-image"] = cfg.PauseImage
- }
```

**Note:** The same flag in `pkg/agent/cridockerd/cridockerd.go` was retained, as it is a cri-dockerd parameter, not a kubelet flag.

### 23. `CloudDualStackNodeIPs` Feature Gate Removed in Kubernetes 1.35

**Error:**
```
ERRO cloud-controller-manager exited: invalid argument "CloudDualStackNodeIPs=true"
for "--feature-gates" flag: unrecognized feature gate: CloudDualStackNodeIPs
```

**Root Cause:** The `CloudDualStackNodeIPs` feature gate graduated to GA in a prior release and was removed from the codebase in Kubernetes 1.35. GA-graduated feature gates are cleaned up after a standard deprecation cycle.

**Affected File:** `pkg/daemons/control/server.go`

**Resolution:** Remove the feature gate from cloud-controller-manager args:

```diff
  argsMap := map[string]string{
      ...
      "bind-address": cfg.Loopback(false),
-     "feature-gates": "CloudDualStackNodeIPs=true",
  }
```

### 24. metrics-server Upgrade v0.6.3 → v0.8.1

**Error:**
```
loading OpenAPI spec for "v1beta1.metrics.k8s.io" failed with: failed to download
v1beta1.metrics.k8s.io: ResponseCode: 503, Body: service unavailable
```

**Root Cause:** metrics-server v0.6.3 is incompatible with Kubernetes 1.35. The metrics-server compatibility matrix shows v0.8.x is required for Kubernetes 1.31+.

**Affected Files:**
- `manifests/metrics-server/metrics-server-deployment.yaml`
- `manifests/metrics-server/metrics-server-service.yaml`
- `manifests/metrics-server/aggregated-metrics-reader.yaml`
- `pkg/deploy/zz_generated_bindata.go` (regenerated)

**Resolution:** Upgrade to metrics-server v0.8.1 with the following changes:

| Setting | v0.6.3 | v0.8.1 |
|---------|--------|--------|
| Image tag | `v0.6.3` | `v0.8.1` |
| `maxUnavailable` | `1` | `0` |
| `priorityClassName` | `system-node-critical` | `system-cluster-critical` |
| `nodeSelector` | (none) | `kubernetes.io/os: linux` |
| Memory request | `70Mi` | `200Mi` |
| Readiness `initialDelaySeconds` | `0` | `20` |
| `--tls-cipher-suites` | explicit list | removed (secure defaults built-in) |
| `seccompProfile` | (none) | `RuntimeDefault` |
| `capabilities.drop` | (none) | `ALL` |
| Service `appProtocol` | (none) | `https` |

Run `go generate` on Linux to regenerate `pkg/deploy/zz_generated_bindata.go`.

### 25. `genClientCerts` Cyclomatic Complexity Refactoring

**Issue:** DeepSource flagged `genClientCerts` with cyclomatic complexity of 24 (GO-R1005, "high" risk).

**Root Cause:** The function contained 7 repetitive blocks of certificate generation + optional kubeconfig writing, each with its own error handling branches.

**Affected File:** `pkg/daemons/control/deps/deps.go`

**Resolution:** Extracted a `certKeyKubeConfig` struct and `generateCertAndKubeConfig` helper function, replacing 7 repetitive blocks with a declarative config slice and loop:

```go
type certKeyKubeConfig struct {
    commonName string
    orgs       []string
    certFile   string
    keyFile    string
    kubeConfig string // if empty, no kubeconfig is generated
}

func generateCertAndKubeConfig(factory signedCertFactory, c certKeyKubeConfig, apiEndpoint, serverCA string) error {
    certGen, err := factory(c.commonName, c.orgs, c.certFile, c.keyFile)
    if err != nil { return err }
    if certGen && c.kubeConfig != "" {
        return KubeConfig(c.kubeConfig, apiEndpoint, serverCA, c.certFile, c.keyFile)
    }
    return nil
}
```

### 26. Scheduler Deadlock with `waitForUntaintedNode`

**Error:** All pods stuck in `Pending` state. Scheduler process starts but port 10259 never opens. No scheduling occurs.

**Root Cause:** A three-way deadlock between the scheduler, CNI, and cloud-controller-manager:

1. **Scheduler** calls `waitForUntaintedNode()` before starting, waiting for CCM to remove the `node.cloudprovider.kubernetes.io/uninitialized` taint from at least one node
2. **`waitForUntaintedNode`** uses the kubelet kubeconfig (`KubeConfigKubelet`), but the Node Authorizer restricts kubelet to only accessing its own Node object — `List` and `Watch` on all nodes are forbidden, so the watch silently fails
3. **CNI (cilium)** pods are Pending because no scheduler is running to schedule them
4. **Node stays NotReady** because CNI is not initialized
5. **CCM cannot remove the taint** because the node is not ready → back to step 1

**Affected File:** `pkg/daemons/executor/embed.go`

**Resolution:** Removed the `waitForUntaintedNode` call from the scheduler startup path. The scheduler does not need to wait for untainted nodes — critical DaemonSet pods (CNI, etc.) configure `tolerations: [{operator: Exists}]` which allows scheduling on tainted nodes. Also removed the now-unused `waitForUntaintedNode` and `getCloudTaint` functions and their associated imports.

```diff
  go func() {
      <-apiReady
      for e.nodeConfig == nil {
          runtime.Gosched()
      }
-     if !e.nodeConfig.AgentConfig.DisableCCM {
-         if err := waitForUntaintedNode(ctx, e.nodeConfig.AgentConfig.KubeConfigKubelet); err != nil {
-             logrus.Fatalf("failed to wait for untained node: %v", err)
-         }
-     }
      defer func() {
```

### 27. Bootstrap Token Mismatch on Re-initialization

**Error:**
```
level=fatal msg="Failed to reconcile with temporary etcd: bootstrap data already
found and encrypted with different token"
```

**Root Cause:** This is an operational error, not a code bug. The etcd bootstrap data is encrypted using the cluster token (`K8E_TOKEN`) as the encryption key. When the server is restarted with a different token than the one used during initial cluster creation, the stored bootstrap data cannot be decrypted. The token comparison in `pkg/cluster/storage.go` (`getBootstrapKeyFromStorage`) correctly detects the mismatch and returns an error.

**Resolution (operational):** Either use the original token, or clear the data directory to reinitialize:

```bash
systemctl stop k8e
rm -rf /var/lib/rancher/k8e/server/db
systemctl start k8e
```

## Changes Summary

### go.mod

| Change | Action |
|--------|--------|
| `go.etcd.io/etcd/client/v2` replace | Removed |
| `go.etcd.io/etcd/client/v2` require | Removed |
| `go.etcd.io/etcd/raft/v3` require | Removed |
| `k8s.io/externaljwt` replace | Added |
| `k8s.io/externaljwt` require | Added |
| `opencontainers/runtime-spec` replace → v1.2.1 | Added |
| `opencontainers/cgroups` replace → v0.0.4 | Added |
| `containerd/cgroups v1.1.0` direct require | Removed |
| `containerd/cgroups/v3 v3.1.0` | Promoted to direct |
| `containerd/containerd v1.7.23` | Removed |
| `containerd/containerd/v2 v2.1.5-k3s1` | Added (direct) |
| `containerd/aufs v1.0.0` | Removed |
| `containerd/errdefs v1.0.0` | Added (direct) |
| `containerd/fuse-overlayfs-snapshotter v1.0.8` | Removed |
| `containerd/fuse-overlayfs-snapshotter/v2 v2.1.6` | Added |
| `containerd/zfs v1.1.0` | Removed |
| `containerd/zfs/v2 v2.0.0-rc.0` | Added |
| `urfave/cli/v2` | Promoted to direct |

### Source Files

| File | Change |
|------|--------|
| `pkg/agent/containerd/config_linux.go` | `runc/libcontainer/userns` → `moby/sys/userns`; containerd v1 → v2 client, overlayutils, fuse-overlayfs/v2 |
| `pkg/agent/containerd/config_windows.go` | containerd v1 → v2 client |
| `pkg/agent/containerd/containerd.go` | containerd v1 → v2 (client, images, namespaces, errdefs), local CRI constants |
| `pkg/daemons/agent/agent_linux.go` | `runc/libcontainer/userns` → `moby/sys/userns`; removed `--pod-infra-container-image` kubelet flag |
| `pkg/rootless/rootless.go` | `runc/libcontainer/cgroups` → `opencontainers/cgroups` |
| `pkg/cgroups/cgroups_linux.go` | `containerd/cgroups` v1/v2 → `containerd/cgroups/v3`, `LoadManager` → `Load` |
| `pkg/ctr/main.go` | `containerd/containerd/cmd/ctr` → `containerd/v2/cmd/ctr`, `urfave/cli` → `cli/v2` |
| `pkg/containerd/builtins.go` | All plugin imports → containerd v2 paths, added new v2 plugins |
| `pkg/containerd/builtins_linux.go` | Removed aufs, added erofs/blockfile, fuse-overlayfs/v2, zfs/v2 |
| `pkg/containerd/builtins_windows.go` | containerd v2 diff/snapshots paths |
| `pkg/containerd/builtins_cri.go` | Split CRI import into 3 v2 sub-imports |
| `pkg/containerd/main.go` | v2 command path, removed `seed.WithTimeAndRand()` |
| `pkg/containerd/utility_linux.go` | v2 overlayutils, fuse-overlayfs/v2 |
| `pkg/spegel/bootstrap.go` | Rewritten for new Bootstrapper interface (`Get(ctx)`, `Run(ctx, AddrInfo)`) |
| `pkg/spegel/spegel.go` | Updated for new spegel OCI/registry/routing APIs |
| `pkg/spegel/store.go` | New: deferred OCI store (ported from k3s) |
| `pkg/daemons/executor/embed.go` | `NewSchedulerCommand()` → `NewSchedulerCommand(ctx.Done())`; removed `waitForUntaintedNode` deadlock |
| `pkg/daemons/control/deps/deps.go` | EgressSelector internal types → `v1beta1` versioned types; `genClientCerts` complexity refactoring |
| `pkg/daemons/agent/agent_windows.go` | Removed `--pod-infra-container-image` kubelet flag |
| `pkg/daemons/control/server.go` | Removed `CloudDualStackNodeIPs` feature gate from CCM args |
| `manifests/metrics-server/metrics-server-deployment.yaml` | Upgraded metrics-server v0.6.3 → v0.8.1 with security and resource changes |
| `manifests/metrics-server/metrics-server-service.yaml` | Added `appProtocol: https` |
| `manifests/metrics-server/aggregated-metrics-reader.yaml` | Added `k8s-app: metrics-server` label |
| `pkg/etcd/etcd.go` | `etcdserver.ErrNoLeader` → `etcderrors.ErrNoLeader`, `credentials.NewBundle` → `NewTransportCredential` |
| `pkg/etcd/snapshot.go` | `snapshotv3.Save` → `snapshotv3.SaveWithVersion` |
| `pkg/cluster/storage.go` | `ErrGPRCNotSupportedForLearner` → `ErrGRPCNotSupportedForLearner` (typo fix) |
| `pkg/etcd/etcd_test.go` | `etcdserver.ErrNotEnoughStartedMembers` → `etcderrors.ErrNotEnoughStartedMembers`, `etcdserver.ErrNoLeader` → `etcderrors.ErrNoLeader` |
| `pkg/etcd/resolver.go` | Removed always-true `r.CC != nil` check (`CC` became a method in newer grpc) |
| `pkg/node/controller.go` | Non-constant format string: `logrus.Errorf("..." + var)` → `logrus.Errorf("...%s", var)` |
| `pkg/clientaccess/token.go` | Non-constant format string: `logrus.Warnf(warning.Text)` → `logrus.Warnf("%s", warning.Text)` |
| `pkg/cluster/bootstrap.go` | Non-constant format string: `logrus.Infof("..." + var)` → `logrus.Infof("...%s", var)` |
| `pkg/util/gates_test.go` | Non-constant format string: `t.Errorf("..." + var)` → `t.Errorf("...%s", var, var)` |

### Build System: Makefile → Zig Consolidation

As part of this upgrade, the build system was consolidated to use `build.zig` as the single source of truth for all build logic. The `Makefile` becomes a thin proxy that delegates every target to `zig build <step>`.

#### Makefile Changes

All targets now delegate to zig:

```makefile
all:           zig build all
k8e:           zig build k8e
clean:         zig build clean
deps:          zig build deps
format:        zig build fmt
generate:      zig build generate
package:       zig build package
package-cli:   zig build package-cli
package-airgap: zig build package-airgap
test:          zig build test
```

#### build.zig New Steps

| Step | Command | Environment |
|------|---------|-------------|
| `deps` | `go mod tidy` | — |
| `fmt` | `go fmt ./...` + `zig fmt build.zig` | — |
| `test` | `go test -v ./...` | `CGO_ENABLED=1`, `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn` |
| `generate` | `bash hack/generate` | — |
| `package` | `bash hack/package` | — |
| `package-cli` | `bash hack/package-cli` | — |
| `package-airgap` | `bash hack/package-airgap.sh` | — |
| `clean` | `rm -rf bin dist build .zig-cache zig-out .cni-build` | — |

#### hack/ Directory Cleanup

Removed 5 scripts whose functionality is fully covered by `build.zig`:

| Removed Script | Original Purpose | Replacement |
|---|---|---|
| `hack/build` | Full build (k8e + shim + runc + cni, version injection, symlinks) | `zig build all` (`buildGoBinary` + shim/runc/cni steps) |
| `hack/ci` | Orchestrate download → validate → build → package → size check | Individual `zig build` steps |
| `hack/clean` | `rm -rf dist bin build` | `zig build clean` |
| `hack/validate` | `go mod tidy` + `go generate` + dirty check + lint | `zig build deps` + `zig build generate` |
| `hack/package` | Check bin exists → call build → call package-cli | Workflow splits into `make k8e` → `make package-cli` |

Retained scripts (still called by `build.zig` or other scripts):

| Script | Purpose | Called By |
|---|---|---|
| `hack/version.sh` | Version detection from git/go.mod | `build.zig` (`getVersionEnv`), `package-cli`, `download` |
| `hack/download` | Clone runc/containerd repos, download nerdctl/cilium | `zig build download` |
| `hack/generate` | `go generate` | `zig build generate` |
| `hack/package-cli` | Package final release binary (symlinks, checksums, tar+zstd) | `zig build package-cli` |
| `hack/package-airgap.sh` | Package air-gap images | `zig build package-airgap` |
| `hack/binary_size_check.sh` | Validate k8e binary < 128MB | Utility (not yet in zig) |
| `hack/boilerplate.go.txt` | Code generation template | `go generate` |
| `hack/crdgen.go` | CRD generation source | `go generate` |
| `hack/airgap/` | Image list for air-gap packaging | `package-airgap.sh` |

#### CI Workflow Updates

All 3 GitHub Actions workflows (`.github/workflows/`) updated:

| Workflow | Changes |
|---|---|
| `testing.yml` | `go mod tidy` → `make deps`; `go test` → adds `CGO_ENABLED=1` + `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn`; Install deps: `libc6-dev zlib1g-dev libseccomp-dev` |
| `release.yml` | Added `make deps` step; `make generate` now runs via zig; Install deps: `libc6-dev zlib1g-dev libseccomp-dev` |
| `builder-arm64.yaml` | Added `make deps` step; Added install deps step with apt/yum fallback for Ubuntu and Amazon Linux |

**Install dependencies step** (required for static linking):

```yaml
# Ubuntu runners (testing.yml, release.yml)
- name: Install dependencies
  run: sudo apt-get update && sudo apt-get install -y libc6-dev zlib1g-dev libseccomp-dev

# Self-hosted ARM64 runner (builder-arm64.yaml, may be Ubuntu or Amazon Linux)
- name: Install dependencies
  run: sudo apt-get update && sudo apt-get install -y libc6-dev zlib1g-dev libseccomp-dev || sudo yum install -y glibc-static zlib-static libseccomp-static libseccomp-devel
```

## Known Expected Build Artifacts

- `cmd/k8e/main.go` — `data.AssetNames`/`data.Asset` undefined (generated by `go-bindata` during full build pipeline)
- `zz_generated_list_types.go` — If stale imports appear, run `go clean -cache`

### Testing Requirements

Tests require specific environment variables due to two known issues:

```bash
# Recommended: use the Makefile target
make test

# Or run manually:
CGO_ENABLED=1 GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn go test ./...
```

- **`CGO_ENABLED=1`** — Required because `github.com/erikdubbelboer/gspt` (process title) uses cgo on Linux. Without it, `pkg/proctitle` and all `pkg/cli/*`/`cmd/*` packages that import it will fail with "build constraints exclude all Go files" (see §17).
- **`GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn`** — Required because containerd v1 and v2 coexist in the module graph, causing duplicate protobuf registration of `containerd.runc.v1.ProcessDetails`. Without it, `pkg/agent/config`, `pkg/agent/containerd`, and `pkg/cluster` tests will panic (see §17 Protobuf section below).

### Protobuf Namespace Conflict (containerd v1 + v2 coexistence)

When running `go test ./...`, packages that transitively import the containerd v1 root package (`github.com/containerd/containerd`) will panic:

```
panic: proto: file "github.com/containerd/containerd/runtime/v2/runc/options/oci.proto"
has a name conflict over containerd.runc.v1.ProcessDetails
previously from: "github.com/containerd/containerd/api/types/runc/options"
currently from:  "github.com/containerd/containerd/runtime/v2/runc/options"
```

**Root Cause:** Both containerd v1 (`v1.7.23`) and the separate `containerd/containerd/api` module (`v1.9.0`, needed by containerd/v2) register the same protobuf message `containerd.runc.v1.ProcessDetails`. The v1 root package's `container.go`, `task.go`, and `task_opts.go` transitively import `runtime/v2/runc/options`, causing the conflict.

**Affected packages:** `pkg/agent/config`, `pkg/agent/containerd`, `pkg/cluster` (any test binary that transitively imports containerd v1 root).

**Workaround:** Use `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn` (or run `make test`):

```bash
GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn go test ./...
```

**Proper fix:** Migrate all containerd v1 root package imports to containerd/v2 (see Future Work).

## Future Work

- **spegel full feature parity**: k3s added auth middleware (`MaxInFlight`), JSON peer list responses, deferred containerd start on CRI ready, and `P2pMulAddrAnnotation` node patching. Some of these features were ported, but node annotation patching in `agentBootstrapper.Run` is simplified.

## Reference

- Upstream k3s go.mod: `github.com/k3s-io/k3s` branch `release-1.35`
- etcd v3.6.x changelog: raft extracted to `github.com/etcd-io/raft`, `client/v2` removed
- runc v1.4.0 release notes: `libcontainer/userns` → `github.com/moby/sys/userns`, `libcontainer/cgroups` → `github.com/opencontainers/cgroups`
- Kubernetes v1.35.1: new staging module `k8s.io/externaljwt`, `NewSchedulerCommand` signature change, `--pod-infra-container-image` removed, `CloudDualStackNodeIPs` feature gate removed
- opencontainers/runtime-spec v1.3.0: `LinuxPids.Limit` changed from `int64` to `*int64`
- opencontainers/cgroups v0.0.6: `Resources.PidsLimit` changed from `int64` to `*int64`
- containerd/cgroups v3: `LoadManager` → `Load`, gogo/protobuf → google/protobuf
- spegel v0.6.0-k3s1: Bootstrapper interface rewrite, OCI client/store split, registry API changes
- etcd v3.6.7: `Save` → `SaveWithVersion`, `ErrNoLeader` moved, `credentials.NewBundle` removed, `ErrGPRC` typo fixed
- Go 1.25 vet: non-constant format strings in printf-like functions now rejected
- grpc `resolver/manual`: `CC` field changed to method, nil comparison always true
- gspt: requires `CGO_ENABLED=1` on Linux (uses cgo for `prctl` syscall)
- Protobuf namespace conflict FAQ: https://protobuf.dev/reference/go/faq#namespace-conflict
- k3s build script: `github.com/k3s-io/k3s/blob/release-1.35/scripts/build`
- containerd v2 migration: `github.com/containerd/containerd/v2` module layout
- metrics-server compatibility matrix: https://github.com/kubernetes-sigs/metrics-server#compatibility-matrix
- k8s.io/apiserver EgressSelectorConfiguration: internal types lack JSON tags, must use versioned types for serialization
