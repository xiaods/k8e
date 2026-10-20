# KIP-31: E2B Template Semantics for K8E Sandboxes

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-10-20 | Proposed — M0 (template registry) is the ship-first slice; M1–M3 add the E2B build API, the golden workspace and the start command; M4–M5 add tags/cache/lifecycle and the native (non-E2B) surface |

## Summary

[KIP-18](kip-18-sandbox-e2b-compat.md) made the **official `e2b` SDK** talk to
K8E unchanged — but it treats the E2B `templateID` as nothing more than an
alias for a K8s **RuntimeClass**: `gvisor` / `kata` / `firecracker` are
accepted as template names, `base` means "the default runtime", everything else
is `404 template not found` (`pkg/sandbox/e2b/control.go:56`,
`resolveRuntimeClass`). KIP-18 §12 lists *"Template registry: map E2B template
names to k8e session images"* as an open item — this KIP closes it.

E2B's template model is much richer than a runtime class. Per the E2B
[template quickstart](https://docs.e2b.dev/template/quickstart) and
[how-it-works](https://docs.e2b.dev/template/how-it-works), a **template is a
build-time artifact**:

```
base image ──▶ layers (COPY / RUN / ENV / WORKDIR / USER) ──▶ startCmd ──▶ readyCmd
                                                                              │
                                                        snapshot (fs + running process) ──▶ template
```

and a sandbox created from it starts with the filesystem **and the long-running
process already up** — "zero wait time for your users". Templates are
versioned (`name:tag`), layered (`fromTemplate`), cached per layer, and
addressed by name from the SDK: `Sandbox.create("my-template:v1")`.

This KIP brings that model to K8E in two layers:

1. **M0 — the template registry.** The `SandboxTemplate` CRD (which exists
   today but is barely wired: `pkg/sandboxmatrix/api/v1alpha1/types.go:145`,
   registered at `pkg/crd/crds.go:38`) becomes the authoritative template
   record — name, tags, base image, runtime class, resources, env, user/workdir,
   start/ready command and (after a build) a prebuilt workspace artifact.
   `Sandbox.create("my-template:v1")` resolves it, and the resolution is
   **tenant-scoped** and **multi-node consistent** because it lives in the
   API server, not in a per-process map.
2. **M1–M4 — the build service.** The E2B template API
   (`POST /v3/templates`, `POST /v2/templates/{id}/builds/{id}`,
   `GET /templates/{id}/builds/{id}/status`, …) is implemented, so
   `Template.build(...)` from the **unmodified** SDK/CLI works against K8E.
   The build runs as a real sandbox session; the artifact it produces is the
   template's **golden workspace**, seeded into every session created from
   that template.

Everything K8E cannot reproduce is stated honestly (§7): a K8E template does
**not** capture process state, so the start command is *re-run* per sandbox
instead of being restored already-running. The rest of the E2B template
semantics — names, tags, layers, caching, build logs, build limits — map
cleanly.

**This KIP is a revision of KIP-18's template handling, not a rewrite of
KIP-18.** KIP-18's protocol layer, auth model, error dialect and process
surface are unchanged; what changes is that `templateID` stops being a
runtime-class alias and becomes a **first-class template reference**
(§3, §6).

## Motivation

- **Ecosystem interop is incomplete without templates.** The E2B quickstart's
  whole premise is `Template.build(...)` → `Sandbox.create(templateName)`.
  An agent that ships an E2B template (Claude Code, Next.js, a seeded
  database, a pinned Python env) cannot use K8E today: the create call
  404s with `template 'my-template' not found`. KIP-18 made the *runtime*
  work; this KIP makes the *environment* work.
- **K8E already has the two halves, separately.** The pod spec is fully
  parameterizable (`SandboxPodSpec`, `pkg/sandboxmatrix/grpc/orchestrator.go:1052`)
  and the workspace is a PVC — but the pod image is **hard-coded**
  (`sandboxImage = "ghcr.io/xiaods/k8e-sandbox:latest"`,
  `orchestrator.go:90`) and the only prebuilt-content mechanism is a
  **client-side** snapshot tarball (`pkg/sandboxcli/snapshot.go`). What is
  missing is the *server-side, named, versioned* artifact that ties them
  together.
- **The warm pool needs a template hook.** KIP-25's `SandboxWarmPool` already
  has a `templateRef` field (`types.go:117`) and the reconciler ignores it
  (`pkg/sandboxmatrix/controller.go:359`, `reconcileSinglePool`). A template
  is exactly what a warm pool should be parameterized by: *which image, which
  runtime, which prebuilt workspace*.
- **Agent ergonomics.** "Same environment every time" is the property agents
  actually need (reproducible runs, cached deps, a server already listening).
  Today the only reproducible thing in K8E is "the same image", which the
  sandbox image does not let you vary.

## 1. What an E2B template is (source of truth)

Read from the E2B docs (quickstart, how-it-works, defining-template,
start-ready-command, build, caching, names, tags) and from the **official SDK
v2.56 wire format** (`e2b` npm package, `Template.build`):

### 1.1 Definition (build-time)

| Layer | E2B API | Notes |
|-------|---------|-------|
| base image | `fromBaseImage()` / `fromUbuntuImage("22.04")` / `fromTemplate("base")` | `fromTemplate` = layer on an existing template; private registries carry credentials |
| copy files | `.copy(src, dest, {user, mode})` | content-hashed (`filesHash`), uploaded as a tar before the build |
| run commands | `.runCmd(cmd, {user})` | also how `remove` / `rename` / `makeSymlink` are expressed |
| install packages | `.pipInstall` / `.npmInstall` / `.bunInstall` / `.aptInstall` | sugar over `runCmd` |
| git | `.gitClone(url, path, {branch, depth})` | sugar over `runCmd` |
| env | `.setEnvs({K: V})` | **build-time only** — E2B explicitly warns these are not injected into sandboxes created later |
| user / workdir | `.setUser("node")` / `.setWorkdir("/app")` | last value is persisted as the sandbox default |
| start command | `.setStartCmd(cmd, readyCmd)` | runs **at the end of the build**; the running process is captured in the snapshot |
| ready command | `.setReadyCmd(readyCmd)` | polled until exit 0; default 20 s wait when a start command exists |
| cache control | `.skipCache()` / `forceUpload` / `skipCache` on build | per-layer invalidation |

Helpers for readiness: `waitForPort`, `waitForURL`, `waitForFile`,
`waitForProcess`, `waitForTimeout`.

### 1.2 Build semantics

1. Start a sandbox from the base image.
2. Execute the layers in order.
3. Run the start command (if any) and wait for readiness (default 20 s).
4. **Snapshot the sandbox — filesystem *and* running processes** — and register
   it as the template. Loading it later takes ~80 ms with the processes alive.

Templates start faster and cheaper than snapshots because the guest OS is
restarted *before* the long-running process is captured.

### 1.3 Naming, tags, limits

- Name: trimmed, lowercased, `^[a-z0-9-_]+$`, 1–128 chars. Scoped to a
  project: `your-project/my-template`.
- Tags: `name:tag` (`my-template:v1.0.0`); `name` ≡ `name:default`; a build
  can be pinned as `name:<buildID>`; tags can be assigned/removed without a
  rebuild (`Template.assignTags` / `removeTags` / `getTags`).
- Layer cache: keyed by the layer command + its inputs; scoped to the project;
  `skipCache()` invalidates from that layer on; `forceUpload` also busts the
  *file* cache.
- Build limits: ≤ 1 h, ≤ 8 vCPU, ≤ 8 GiB RAM, ≤ 10 GiB disk, ≤ 20 concurrent
  builds (Hobby/Pro tiers).

### 1.4 The wire protocol the SDK speaks (v2.56)

This is the contract K8E must satisfy for `Template.build` to work
unmodified:

| Call | Request | Response |
|------|---------|----------|
| `POST /v3/templates` | `{name, tags[], cpuCount, memoryMB, minFreeDiskMb}` | `202 {templateID, buildID, public, aliases, names, tags}` |
| `GET /templates/{templateID}/files/{hash}` | `?path=` | `{present, url, headers}` — an upload link; the SDK `PUT`s a gzipped tar to `url` |
| `POST /v2/templates/{templateID}/builds/{buildID}` | `{fromImage\|fromTemplate, fromImageRegistry?, force?, steps[], startCmd?, readyCmd?}` | `202` (build accepted) |
| `GET /templates/{templateID}/builds/{buildID}/status` | `?logsOffset=` | `{buildID, templateID, status: building\|waiting\|ready\|error, logEntries[], logs?, reason?}` |
| `GET /templates/{templateID}/tags` | — | `{buildID, tags[]}` |
| `POST /templates/tags` | `{target, tags[]}` | `{buildID, tags[]}` |
| `DELETE /templates/tags` | `{name, tags[]}` | `204` |
| `GET /templates/aliases/{alias}` | — | `200`/`403` (exists) / `404` (free) |
| `DELETE /templates/{templateID}` | — | `204` |

A build step is `{type, args[], filesHash?, force?}` with `type ∈ {COPY, RUN,
ENV, WORKDIR, USER}` — the SDK's own instruction set (Dockerfile-shaped):

| type | args | meaning |
|------|------|---------|
| `COPY` | `[src, dest, user, mode]` | unpack the uploaded tar (`filesHash`) at `dest` |
| `RUN` | `[command, user?]` | shell command in the build sandbox |
| `ENV` | `[k1, v1, k2, v2, …]` | environment for later layers |
| `WORKDIR` | `[dir]` | working directory for later layers |
| `USER` | `[user]` | user for later layers |

`Sandbox.create(template)` sends `templateID` in the create body — the same
field KIP-18 already parses (`pkg/sandbox/e2b/control.go:21`).

## 2. The K8E template model

### 2.1 Two CRDs: a declaration and its builds

Mirroring K8s conventions (a Deployment and its ReplicaSets), templates are
**declarations** and builds are **runs**:

```go
// SandboxTemplateSpec — the reusable, named environment a session is created
// from. Every field is optional; an empty spec means "the default template"
// (the cluster's default image + default runtime class).
type SandboxTemplateSpec struct {
    Image          string              `json:"image,omitempty"`      // sandbox image (must ship sandboxd)
    RuntimeClass   string              `json:"runtimeClass,omitempty"`// gvisor | kata | firecracker
    ResourceLimits corev1.ResourceList `json:"resourceLimits,omitempty"`
    Env            map[string]string   `json:"env,omitempty"`         // baked into every session
    Workdir        string              `json:"workdir,omitempty"`      // default exec workdir
    User           string              `json:"user,omitempty"`         // default exec user
    StartCmd       string              `json:"startCmd,omitempty"`     // run at session start (§4.4)
    ReadyCmd       string              `json:"readyCmd,omitempty"`     // readiness gate for StartCmd
    Parent         string              `json:"parent,omitempty"`       // base template (E2B fromTemplate)
    Tags           []string            `json:"tags,omitempty"`         // version labels
    Steps          []TemplateStep      `json:"steps,omitempty"`        // recorded build steps
}

// SandboxTemplateStatus is what the build controller resolves the template
// into: the artifact a session is seeded from, and the last build.
type SandboxTemplateStatus struct {
    Phase     string            `json:"phase,omitempty"`  // Ready | Building | Error
    Workspace *TemplateWorkspace `json:"workspace,omitempty"`
    LastBuild string            `json:"lastBuild,omitempty"` // the last build ID
    BuiltAt   *metav1.Time      `json:"builtAt,omitempty"`
}

type TemplateWorkspace struct {
    // ArtifactPVC holds the exported golden workspace tar.
    ArtifactPVC string `json:"artifactPVC,omitempty"`
    // Digest is the sha256 of the exported tar (content addressing).
    Digest string `json:"digest,omitempty"`
    // SizeBytes is the tar size (seed cost).
    SizeBytes int64 `json:"sizeBytes,omitempty"`
    // BuiltFrom is the build that produced this workspace.
    BuiltFrom string `json:"builtFrom,omitempty"`
}
```

`SandboxTemplateBuild` (new CRD) is one build attempt. Its spec carries
the E2B build request verbatim, plus the two shared types:

```go
// TemplateStep is one recorded build layer (E2B's instruction set).
type TemplateStep struct {
    Type      string   `json:"type"`               // COPY | RUN | ENV | WORKDIR | USER
    Args      []string `json:"args,omitempty"`      // type-specific arguments (§1.4)
    FilesHash string   `json:"filesHash,omitempty"` // sha256 of the COPY payload
    Force     bool     `json:"force,omitempty"`     // bypass the cache for this layer
}

// TemplateFile is one uploaded COPY payload staged in the artifact volume.
type TemplateFile struct {
    Hash      string `json:"hash"`                 // sha256 (the E2B filesHash)
    Source    string `json:"source,omitempty"`     // the original relative path
    SizeBytes int64  `json:"sizeBytes,omitempty"`
    Staged    bool   `json:"staged,omitempty"`     // the payload is in the staging area
}
```

```go
type SandboxTemplateBuildSpec struct {
    TemplateRef  string          `json:"templateRef"`             // SandboxTemplate name
    FromImage    string          `json:"fromImage,omitempty"`     // E2B fromImage
    FromTemplate string          `json:"fromTemplate,omitempty"`  // E2B fromTemplate
    Steps        []TemplateStep  `json:"steps,omitempty"`
    StartCmd     string          `json:"startCmd,omitempty"`
    ReadyCmd     string          `json:"readyCmd,omitempty"`
    Force        bool            `json:"force,omitempty"`         // E2B force / skipCache
    CPUCount     int32           `json:"cpuCount,omitempty"`
    MemoryMB     int32           `json:"memoryMB,omitempty"`
    MinFreeDiskMB int32          `json:"minFreeDiskMb,omitempty"`
    Tags         []string        `json:"tags,omitempty"`
    Files        []TemplateFile  `json:"files,omitempty"`         // uploaded COPY payloads
}

type SandboxTemplateBuildStatus struct {
    Phase     string      `json:"phase,omitempty"`   // Pending | Building | Ready | Error | Expired
    BuildID   string      `json:"buildID,omitempty"`
    SessionID string      `json:"sessionID,omitempty"`// the build sandbox
    Digest    string      `json:"digest,omitempty"`
    Error     string      `json:"error,omitempty"`
    StartedAt *metav1.Time `json:"startedAt,omitempty"`
    EndedAt   *metav1.Time `json:"endedAt,omitempty"`
    LogLines  int         `json:"logLines,omitempty"` // log cursor for logsOffset
}
```

`status.phase` mirrors the E2B build status vocabulary
(`building` / `waiting` / `ready` / `error`) so the SDK's polling loop
(`waitForBuildFinish`) works unmodified.

### 2.2 The golden workspace (the "template artifact")

A built template's prebuilt content is stored as **one tar of the build
sandbox's `/workspace`**, exported at the end of the build into a dedicated
**artifact volume** (`sandbox.k8e.io/template-artifacts` PVC) and referenced by
sha256. The template's `status.workspace.digest` is the content address; the
CRD carries the PVC name and the size.

Why a PVC and not the CAS layer store (`pkg/sandboxlayer/layerstore.go`)? The
layer store is **host-local** (`--layer-store-dir`), so an artifact written on
node A is unreachable from node B's sandbox pods. A cluster volume is the only
artifact store that is (a) reachable from every sandbox pod and (b) survives a
server restart. The CAS store stays what it is today (client-side snapshot
dedup); the template artifact is a separate, explicitly-shared thing.

**Access mode is the honest constraint**: with `ReadWriteMany` the artifact is
cluster-wide; with `ReadWriteOnce` (the default for most CSI drivers) the
artifact is pinned to one node and template-backed sessions must be scheduled
there (node affinity). The build controller reports the artifact's node in the
template status, and the session scheduler adds the affinity. This is a
deployment-time decision, surfaced in the KIP rather than hidden.

## 3. Name resolution

`Sandbox.create("<ref>")` sends `templateID`; the reference travels with the
create request and is resolved **server-side, by the gateway**, against the
`SandboxTemplate` CRD (so every node resolves it identically, and the native
gRPC surface gets templates for free — §6.2):

| Reference | Meaning | K8E resolution |
|-----------|---------|----------------|
| `base` (or empty) | the default template | the cluster's default image + default runtime class |
| `gvisor` / `kata` / `firecracker` | **legacy** (KIP-18) runtime-class aliases | still accepted — a runtime class, unchanged behaviour |
| `my-template` | a template named `my-template` | `SandboxTemplate` named `my-template` in the sandbox namespace, tenant-scoped |
| `my-template:v1` | a tagged template | the template whose `status.tags` contains `v1` (or `spec.tags`) |
| `my-template:<buildID>` | a pinned build | the `SandboxTemplateBuild` with that ID, resolved to the template + workspace digest it produced |
| `project/my-template` | a project-scoped template | the template whose `sandbox.k8e.io/project` label matches; the default project is the caller's tenant |
| unknown | — | `404 {code, message: "template 'x' not found"}` — unchanged from KIP-18 |

**Name normalization.** E2B names allow `_` and are lowercased; K8s object
names are DNS-1123 (no `_`). The resolver normalizes an incoming reference
(lowercase, `_`→`-`, trim) before lookup and keeps the original spelling in
the template's `metadata.annotations["sandbox.k8e.io/e2b-alias"]`, so an E2B
name still resolves to the same template. Two templates whose normalized names
collide are rejected at creation time (422) rather than silently shadowing
each other.

**Backward compatibility.** The runtime-class aliases stay exactly as they are
today, so an existing KIP-18 deployment is unaffected. The three runtime-class
names are **reserved**: the alias wins over a template of the same name, and
creating a template with a reserved name is rejected with a clear message.
That keeps the legacy path unambiguous.

## 4. Session creation from a template

```
E2B SDK                    e2b layer                  gateway / orchestrator
   │  POST /v2/sandboxes  │                           │
   │  {templateID:"x:v1"} │──▶ CreateSession{template:"x", templateTag:"v1"}
   │                       │                           │
   │                       │                    resolveTemplate() ──▶ SandboxTemplate (CRD)
   │                       │                           │      ├── image, runtimeClass, resources
   │                       │                           │      ├── env, workdir, user
   │                       │                           │      ├── startCmd, readyCmd
   │                       │                           │      └── workspace digest + artifact PVC
   │                       │                    ┌──────┴───────┐
   │                       │                    │ claim/create  │  pod: template image,
   │                       │                    │ pod + PVC    │  runtime, resources
   │                       │                    └──────┬───────┘
   │                       │                    seed golden workspace (§4.2)
   │                       │                    start startCmd + ready gate (§4.4)
   │  201 {sandboxID…}     │◀────────────────────── session ready
```

The **template resolution happens in the gateway**, not in the e2b layer, so
the native gRPC surface gets templates too (§6.2) and the resolution is
identical on every node (the CRD is the single source of truth).

### 4.1 Pod shape

A template session's pod differs from a default session in exactly four ways:
the **image** (template's), the **runtime class** (template's), the **resource
limits** (template's) and the **workspace seed**. The pod spec is still
`SandboxPodSpec` (`orchestrator.go:1052`) — the template only supplies
parameters to it.

> **Prerequisite (P0).** Today the *session* pod path hard-codes the image
> (`sandboxImage`, `orchestrator.go:90`) while the *warm pool* path honors
> `--sandbox-default-image` (`controller.go:877`, `warmPodSpec`). M0 must
> make both paths honor the resolved template image, otherwise a template can
> only be used by cold-starting a pod that then runs the wrong image. This is
> a one-line unification of the two call sites plus threading the image
> through `claimOrCreatePod` → `createSessionWithTTL`.

### 4.2 Workspace seeding

When the template carries a golden workspace, the session's workspace must
contain it **before the client can observe the session**:

1. The controller resolves the template's `status.workspace` → artifact PVC +
   digest.
2. The session's workspace PVC (or the claimed warm pod's PVC) is seeded: a
   helper pod (same sandbox image) mounts the artifact PVC read-only, streams
   `/workspace.tar` out; the controller pushes it into the session pod through
   the existing file-write path (`sandboxdClient` → `/files/write`), which
   untars it into `/workspace`.
3. The session is only marked `Active` (and the create call only returns 201)
   after the seed completes.

**Cost**: O(artifact size) through the API. A template's workspace is
expected to be small (deps, config files, a seeded DB — typically < 100 MiB);
the CRD records `sizeBytes` and the create path enforces a configurable
ceiling (`--template-max-workspace-mb`, default 512). Larger environments
should be expressed as a **pre-baked image** (§7, D4) rather than a workspace
artifact.

### 4.3 Warm pool interaction

A warm pod is a blank, pre-booted pod with its own PVC. Two options:

- **Cold path** — a template session that finds no matching warm pod is
  created cold (own PVC, own pod, then seeded). Correct, slower.
- **Warm path (preferred)** — `SandboxWarmPool.spec.templateRef` points at a
  template; the pool's pods then run the template's image/runtime/resources
  and the refill **seeds each new pod's PVC from the golden workspace** at
  claim time. A template session claims only from pools whose template
  matches (the claim filter gains a `sandbox.k8e.io/template` label, next to
  the existing `labelRuntimeClass` filter at `orchestrator.go:989`).

This is what makes "templates start fast" true in K8E: the *pod* is already
running, and the *content* is pre-seeded, so a session from a warm template
is a label flip + a workspace copy.

### 4.4 Start command and readiness

E2B runs the start command **at build time** and captures it running (D1).
K8E cannot capture a process, so the template's `startCmd` is **re-run in
every session created from the template**, and the session is only handed to
the client once the process is up:

1. The controller starts the template's `startCmd` in the session pod (a
   background run, tracked like any other background exec — KIP-11).
2. It waits for the **ready gate**: the template's `readyCmd` (polled until
   exit 0), or the equivalent probe (port open / file exists / process
   alive). The gate is evaluated inside the pod, so it is the pod's own
   network view.
3. The session becomes `Active` and the create call returns `201`. If the
   gate does not pass within the template's `startTimeout` (default 60 s), the
   create call answers **503 + `Retry-After`** (KIP-18's mid-lifecycle
   dialect) and the session keeps starting — the client may retry or inspect
   the transcript.

The pod's own readiness probe stays TCP :2024 (sandboxd liveness); the start
command's gate is a **session-level** property, because a warm pod is booted
once but serves many sessions — the command must be (re-)started per claim,
after the workspace has been re-seeded.

The ready gate is also what makes a template's "server is already listening"
promise observable: `Sandbox.create(...)` returns only when the port is up,
which is the E2B behaviour users depend on, minus the ~80 ms snapshot restore
(K8E pays a process start instead).

## 5. The build service (M1–M2)

### 5.1 Flow

```
Template.build(tpl, "my-template:v1")            e2b layer                build controller
   │  POST /v3/templates {name,tags,cpu,memory}    │                            │
   │──────────────────────────────────────────────▶│ create SandboxTemplate     │
   │                                               │ + SandboxTemplateBuild     │
   │  GET /templates/{tid}/files/{hash}            │                            │
   │──────────────────────────────────────────────▶│ signed upload link →      │
   │  PUT <tar.gz>                                 │ artifact volume (staging) │
   │─────────────────────────────────────────────▶ │                            │
   │  POST /v2/templates/{tid}/builds/{bid}        │ record steps, start build │
   │  {fromImage, steps[], startCmd, readyCmd}     │──────────────▶│           │
   │                                               │               │ create build session (a real sandbox pod)
   │                                               │               │ execute layers in order
   │                                               │               │ run startCmd, wait readyCmd
   │                                               │               │ export /workspace → artifact PVC (tar, sha256)
   │                                               │               │ set build status Ready + digest
   │  GET /templates/{tid}/builds/{bid}/status     │◀──────────────│           │
   │◀──────────────────────────────────────────────│ {status, logEntries}       │
```

### 5.2 Layer execution

Each step is executed **inside a real sandbox session** (the build sandbox) —
the same exec path the CLI uses, so there is no second execution engine:

| step | execution in the build sandbox | recorded in the template |
|------|-------------------------------|-------------------------|
| `COPY` | the uploaded tar is streamed into the pod and untarred at `dest` (honoring `user`/`mode`) | `steps[]` (for rebuild/cache) |
| `RUN` | `exec` of the command as `user` (default: the template's user) | `steps[]` |
| `ENV` | exported for the **remaining** layers of this build (E2B semantics: build-time only) | `spec.env` (applied to future sessions) |
| `WORKDIR` | the working directory for the remaining layers | `spec.workdir` |
| `USER` | the user for the remaining layers | `spec.user` |

**System-level steps are refused, not silently mis-executed.** The sandbox
root filesystem is read-only (`ReadOnlyRootFilesystem: true`,
`orchestrator.go:1087`) and packages install into `/workspace` (KIP-13). So
`apt install` (system packages) **cannot** be captured into a workspace
artifact: the build answers `400 {code, message: "system-level build steps
require a pre-baked image; see KIP-31 §7 D4"}` for a step that writes outside
`/workspace`. `pip`/`npm`/`uv` installs that land in `/workspace` (or a venv
under it) work and are captured. This is the single most important honesty
point of the whole KIP: **K8E templates capture the workspace, not the root
filesystem.**

### 5.3 Layer cache

E2B caches per layer, keyed by the layer command and its inputs. K8E
computes the same layer key, but caches at a coarser granularity — the whole
build:

```
layerKey = sha256(parentDigest || stepType || stepArgs || filesHash)
buildKey = sha256(parentDigest || layerKey₁ || layerKey₂ || …)
```

- The **result** of a cached layer is the workspace content at that point.
  Caching the *content* per layer is expensive (a snapshot per layer), so the
  artifact is cached under `buildKey`: a rebuild with an identical step list
  returns the cached artifact **without running a build sandbox at all**.
- `force` / `skipCache` bypasses the cache; `forceUpload` additionally
  re-uploads the file payload.
- The cache lives next to the artifact (a `cache` key in the artifact volume)
  and is garbage-collected with the templates that reference it.

The coarser cache is an honest, documented divergence from E2B's per-layer
cache (D5): it costs a full rebuild when only a late layer changed, but it is
simple, correct and cheap to implement.

### 5.4 Build limits

E2B's build limits map onto K8s naturally:

| E2B limit | K8E enforcement |
|-----------|------------------|
| ≤ 1 h build duration | `SandboxTemplateBuild` TTL (default 60 min) → the build session is destroyed, status `Expired` |
| ≤ 8 vCPU | `spec.cpuCount` → the build session's CPU limit; requests above the node's capacity are refused at build time (`503 sandbox_capacity_unavailable`) |
| ≤ 8 GiB RAM | `spec.memoryMB` → the build session's memory limit; refused when the node cannot satisfy it |
| ≤ 10 GiB disk | the artifact size ceiling (`--template-max-workspace-mb`) plus the PVC's size |
| ≤ 20 concurrent builds | a cluster-wide build concurrency gate (leader-counted); excess builds wait in `Pending` and are reported as `waiting` (the SDK's polling loop understands `waiting`) |

## 6. Protocol surface

### 6.1 E2B endpoints (new)

Mounted on the same control-plane router as KIP-18's routes
(`pkg/sandbox/e2b/server.go`, `registerControlRoutes`), with the same
`X-API-Key`/`Bearer` auth and the same `{code, message}` error dialect:

| Endpoint | K8E answer |
|----------|-----------|
| `POST /v3/templates` | create the `SandboxTemplate` + `SandboxTemplateBuild`; `202 {templateID, buildID, public:false, aliases:[], names:[…], tags:[…]}` |
| `GET /templates/{templateID}/files/{hash}` | `{present, url, headers}` — a signed upload link into the artifact volume's staging area |
| `POST /v2/templates/{templateID}/builds/{buildID}` | validate + record the build spec, enqueue the build; `202` |
| `GET /templates/{templateID}/builds/{buildID}/status` | `{buildID, templateID, status, logEntries[], reason?}` — the log entries are the build session's exec transcript |
| `GET /templates/{templateID}/tags` | `{buildID, tags[]}` |
| `POST /templates/tags` | `{target, tags[]}` → assign tags to a build |
| `DELETE /templates/tags` | remove tags (the artifact survives — E2B semantics) |
| `GET /templates/aliases/{alias}` | `200` if the name is taken, `404` if free (E2B's `Template.exists`) |
| `GET /v2/templates` | list templates visible to the tenant |
| `DELETE /templates/{templateID}` | delete the template (and its artifact) |

**Log streaming.** E2B's build logs are polled with a cursor
(`?logsOffset=`). K8E answers with the build session's transcript
(`GetTranscript`, KIP-16 M4) sliced at the cursor — the same mechanism the
sandbox transcript already uses, no new plumbing.

### 6.2 Native (non-E2B) surface

Templates are not an E2B-only concept; the native surface gets them too:

- **gRPC**: `CreateSessionRequest` gains `string template = 7;` and
  `string template_tag = 8;` (proto change → `make generate`). The gateway
  resolves the template (server-side), so the CLI, the MCP adapter and the
  dsh plugin can all create template sessions.
- **CLI**: `k8e-sandbox-cli create --template my-template[:tag]` and a
  `k8e-sandbox-cli template` group (`list` / `show` / `build` / `rm`) that
  mirrors the E2B CLI's ergonomics for operators who do not use the SDK.
- **Warm pool**: `SandboxWarmPool.spec.templateRef` becomes meaningful — the
  pool's pods run the template's image and are seeded from its golden
  workspace.

### 6.3 What does *not* change

- The E2B **create** path keeps its shape: `templateID` in the create body,
  `404 template not found` for unknown references, `metadata.name` idempotency,
  deadlines, auto-pause.
- The **info view**'s `templateID` field now reports the **template
  reference** (`my-template:v1`) instead of the runtime class. The E2B info
  model has no runtime field, so nothing is lost — the runtime class stays
  internal (`templateIDFor`, `pkg/sandbox/e2b/gateway.go:212`, is replaced by
  the resolved template). The SDK's generated model only requires the field to
  exist, so this is transparent.

## 7. Honest divergences

Every item below is a place where K8E **cannot** be byte-identical to E2B.
Each one is deliberate and surfaced in the API (a field, a status, an error
message) rather than papered over.

| # | E2B | K8E | How it is surfaced |
|---|-----|-----|--------------------|
| **D1** | The start command is captured **running** in the snapshot; a sandbox starts with the process already up | K8E sandboxes have no process-state snapshot (gVisor/Kata pods are not snapshotted) | The template's `startCmd` is **re-run per session** at start, and the session is only handed out after the ready gate passes. The cost is a per-session startup delay instead of E2B's ~80 ms restore; the benefit is that the process is *live* and crash-visible. A future VM-snapshot backend (Firecracker runtime class) could close this. |
| **D2** | A template captures the **whole filesystem** (root included) | The sandbox root is read-only and the writable surface is `/workspace` (PVC) | The template captures the **workspace** only. System-level build steps (apt, system services) are refused with an explicit `400`, not silently dropped. |
| **D3** | The template's kernel is pinned at build time and cannot change | The kernel is the node's (gVisor 6.x / Kata / Firecracker host kernel) | A template records the runtime class it was built for; a session created with a different runtime class is refused (`409 template runtime class mismatch`) rather than silently running a different kernel. |
| **D4** | `fromImage` + `apt install` produce a template with the packages inside | A K8E template can only pin a **pre-baked image** | The `image` field is the escape hatch: bake the environment into a sandbox image (still shipping `sandboxd`) and reference it. In-cluster image building is explicitly **out of scope** for this KIP. |
| **D5** | Per-layer build cache | Per-**build** cache (the whole step list) | `spec.steps` records the layers; the cache key is the full step-list digest. Documented in §5.3. |
| **D6** | Templates are scoped to a **project**; public templates are shared | Templates are scoped to a **tenant** inside the sandbox namespace | `project/name` references resolve through the `sandbox.k8e.io/project` label; the caller's tenant is the default project. There is no cross-tenant public template registry in this KIP. |
| **D7** | A template build produces an artifact on E2B's infrastructure | The artifact lives in a cluster volume with an access mode | RWX → cluster-wide; RWO → template sessions are pinned to the artifact's node (affinity). Reported in the template status. |
| **D8** | `Template.build` streams logs | K8E polls the build transcript | Same `logsOffset` cursor semantics, so the SDK's polling loop is unchanged. |

## 8. Security

- **Build steps are code execution.** A template build runs arbitrary shell
  commands in a sandbox pod. It therefore runs with the *same* isolation as
  any sandbox session (RuntimeClass, egress allowlist, resource limits) and
  never on the host. The build session is destroyed when the build ends —
  success or failure.
- **Env validation.** The template's `env` is injected into every session
  created from it, so it must pass the same validation KIP-18's appendix A1
  recommends for create-time env vars (name charset, deny-list of
  loader/path variables such as `LD_PRELOAD`, `PYTHONPATH`, `PATH`, value size
  and control characters). A template whose env violates the rules is rejected
  at **build** time, not at session-create time.
- **Registry credentials.** E2B's `fromImageRegistry` carries registry
  credentials in the build request. K8E must **not** persist them in the CRD
  (they would be readable by anyone with CRD read access). They are used for
  the build only and dropped when the build ends; the template records the
  *image reference* only. Private-registry pulls therefore need the node's
  image-pull secret — the build refuses with a clear message when the image
  cannot be pulled.
- **Upload links** are signed and scoped to one build + one content hash, and
  expire (the same scheme as KIP-18's signed file URLs).
- **Tenant isolation.** A template is visible only to its tenant; the
  resolver filters by the tenant label, and the build controller refuses to
  build for a template the caller does not own.

## 9. Milestones

| M | Scope | Concrete changes | Ship criterion |
|---|-------|------------------|----------------|
| **P0** | Prerequisite | Session pods honor `--sandbox-default-image`; thread the image through `claimOrCreatePod` → `createSessionWithTTL` | A cold-started session runs the configured image |
| **M0** | **Template registry** | Extend the `SandboxTemplate` CRD (spec/status above); the template resolver in the **gateway** (the e2b layer forwards the reference unchanged); `template`/`template_tag` on `CreateSessionRequest`; `GET /v2/templates`, `GET /templates/aliases/{alias}`; legacy runtime-class aliases unchanged | `Sandbox.create("my-template")` (a hand-written CRD) creates a session with the template's image/runtime/env; unknown names still 404 |
| **M1** | **Build API** | `POST /v3/templates`, `GET /templates/{id}/files/{hash}`, `POST /v2/templates/{id}/builds/{id}`, `GET /templates/{id}/builds/{id}/status`; `SandboxTemplateBuild` CRD; the build controller (leader-gated) runs the steps in a build session; per-build cache; build limits | `Template.build(tpl, "x:v1")` from the unmodified SDK returns `ready`; `Sandbox.create("x:v1")` works |
| **M2** | **Golden workspace** | Export the build workspace into the artifact volume; seed sessions from it; `--template-max-workspace-mb`; artifact GC | A file created by a build step exists in every session created from the template |
| **M3** | **Start command** | Run `startCmd` at session start; the ready gate (`readyCmd` / port / file / process) gates the session; the create call waits (bounded) and answers `503 + Retry-After` on timeout | `Sandbox.create("x")` returns a session whose start process is already running |
| **M4** | **Tags, list, delete, cache control** | Tag APIs; `GET /v2/templates`; `DELETE /templates/{id}`; `skipCache`/`force`/`forceUpload`; artifact GC | `Template.assignTags` / `getTags` / `removeTags` work; a deleted template's sessions are unaffected |
| **M5** | **Native surface + docs** | `k8e-sandbox-cli template` group; `--template` on `create`; warm-pool `templateRef` seeding; SKILL.md + docs; e2e | An operator can build a template from the CLI and use it from the SDK |

M0 is the ship-first slice: it is small, has no new infrastructure, and turns
the 404 into a working path for hand-authored templates.

## 10. Test plan

- **Unit (Go)**
  - Name normalization + resolution: `base`, runtime-class aliases (legacy),
    `name`, `name:tag`, `name:buildID`, `project/name`, unknown → 404,
    normalization collisions → 422.
  - Step translation: `COPY/RUN/ENV/WORKDIR/USER` → build-session actions;
    a system-level step → the explicit `400`.
  - Cache keys: identical step lists hit the cache; `force`/`skipCache`
    bypass it; a changed `filesHash` misses.
  - Env validation (A1 rules) at build time.
  - Build state machine: `Pending → Building → Ready | Error | Expired`, and
    the E2B status vocabulary mapping.
  - Seeding: a seeded session's workspace contains the artifact; the seed is
    idempotent; the size ceiling is enforced.
- **Gateway / controller**
  - A template session's pod uses the template's image/runtime/resources.
  - The warm-claim filter skips pods from a different template.
  - A pool with `templateRef` seeds its pods.
- **E2E (live cluster)**
  - The **official** SDK: `Template.build(tpl, "e2b-demo:v1")` → poll status →
    `Sandbox.create("e2b-demo:v1")` → assert the built file exists, the start
    process is running, `Template.getTags` works, `Template.delete` works.
  - The **official CLI**: `e2b template create` / `e2b template ls`.
  - The Gateway API path (HTTP :80/:443) as well as loopback.

## 11. Alternatives considered

- **Keep `templateID` as a runtime-class alias and stop here** — rejected: it
  is the "honest absence" KIP-18 already documents, but it leaves the E2B
  quickstart's core workflow (`Template.build` → `Sandbox.create(name)`)
  unusable, which is the interoperability KIP-18 was written for.
- **Implement the E2B build API but keep the artifact client-side** — rejected:
  the artifact must be materialized *by the server* into a session's workspace
  at create time; a client-side artifact cannot be restored by a session the
  client did not create (and it would not survive a session handed to another
  agent).
- **Snapshot the build pod with a CSI VolumeSnapshot** — attractive (it is the
  K8s-native "template" primitive and it *would* capture process state under
  Firecracker), but it requires a CSI driver with snapshot support, which
  k8e's default storage class does not provide. Kept as a future milestone
  (D1/D3) rather than the baseline.
- **Bake every template into an image (in-cluster image builder)** — the most
  faithful mapping (it closes D2 and D4), but it adds a builder (buildah/kaniko)
  and a registry to the deployment — a second distribution surface, against
  k8e's single-binary positioning. The `image` field keeps the door open
  without paying for it.
- **Reuse the CLI's snapshot path verbatim (tar.gz through the file API)** —
  adopted: it is the same mechanism KIP-16 already ships, and the seeding cost
  is acceptable for template-sized workspaces (§4.2).

## 12. Risks / open questions

- **Artifact volume access mode** (D7): RWO pins template sessions to one
  node. A RWX default (or a per-runtime-class artifact volume) is a deployment
  decision; the KIP surfaces the constraint rather than hiding it.
- **Seed cost**: O(artifact size) through the API per session. For very large
  templates this dominates the create latency; the mitigation is the size
  ceiling plus the "use an image" escape hatch.
- **Build-session capacity**: builds compete with real sessions for pool
  capacity. The build concurrency gate and the build TTL bound the damage; a
  dedicated build pool is a possible follow-up.
- **Warm-pool contamination**: a warm pod's workspace is wiped on release
  (sandboxd) — the seed must run *after* the wipe and *before* the client
  sees the session. The existing `resetWorkspace` hook
  (`orchestrator.go:569`) is the natural place to sequence this.
- **Process-state capture (D1)** is only closable with a VM-snapshot backend
  (Firecracker). Until then the honest answer is "the start command runs
  again", and the SDK-visible behaviour is "the process is up when the
  sandbox is handed to you".

## Appendix A — E2B step → K8E action

| E2B step | K8E build action | Captured in the template? |
|----------|------------------|----------------------------|
| `fromImage` | the build session's pod image | ✅ (`spec.image`) |
| `fromTemplate` | the parent template's resolved spec (image + steps + env) | ✅ (inheritance) |
| `COPY src dest` | untar the uploaded payload at `dest` in the build sandbox | ✅ (in the workspace artifact) |
| `RUN cmd` | `exec` in the build sandbox | ✅ (in the workspace artifact) |
| `pip/npm/bun install` | the same, **if** the install lands under `/workspace` | ✅ (KIP-13 isolation) |
| `apt install` / system services | **refused** (`400`) — the root fs is read-only | ❌ (use a pre-baked image) |
| `git clone` | `exec` in the build sandbox | ✅ |
| `ENV` | exported for the remaining build layers | ✅ (`spec.env`, applied to future sessions) |
| `WORKDIR` | the cwd of the remaining build layers | ✅ (`spec.workdir`) |
| `USER` | the user of the remaining build layers | ✅ (`spec.user`; needs `fsGroup` on the PVC) |
| `startCmd` | run at build end **and** re-run per session | ✅ (as a spec; the process is *not* captured — D1) |
| `readyCmd` | the session readiness gate | ✅ |

## Appendix B — E2B template endpoint → K8E answer

| E2B endpoint | K8E answer | Status |
|--------------|-----------|--------|
| `POST /v3/templates` | create template + build | M1 |
| `GET /templates/{id}/files/{hash}` | signed upload link | M1 |
| `POST /v2/templates/{id}/builds/{bid}` | enqueue the build | M1 |
| `GET /templates/{id}/builds/{bid}/status` | build status + logs | M1 |
| `GET /templates/{id}/tags` | tag list | M4 |
| `POST /templates/tags` | assign tags | M4 |
| `DELETE /templates/tags` | remove tags | M4 |
| `GET /templates/aliases/{alias}` | name availability | M0 |
| `GET /v2/templates` | template list | M4 |
| `DELETE /templates/{id}` | delete template + artifact | M4 |
| `Sandbox.create(template)` | `POST /v2/sandboxes` with a resolved template | **M0** (registry) / M2 (artifact) |
