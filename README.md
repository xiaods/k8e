<div align="center">

<img src="docs/assets/k8e-sandbox-logo-light.svg" alt="K8E sandbox" width="440" height="77" />

<br/>

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://golang.org)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue?style=for-the-badge&logo=apache&logoColor=white)](https://github.com/xiaods/k8e/blob/main/LICENSE)
[![Stars](https://img.shields.io/github/stars/xiaods/k8e?style=for-the-badge&logo=github&color=FFD700)](https://github.com/xiaods/k8e/stargazers)
[![Release](https://img.shields.io/github/v/release/xiaods/k8e?style=for-the-badge&logo=github&color=green)](https://github.com/xiaods/k8e/releases)
[![Arch](https://img.shields.io/badge/Arch-x86__64%20%7C%20ARM64%20%7C%20RISC--V-blueviolet?style=for-the-badge)](https://github.com/xiaods/k8e/releases)

<br/>

> **k8e.sh** — Open Source Agentic AI Sandbox Matrix. A **single binary under 100MB** that turns any Linux host into a secure, isolated execution platform for AI agents — gVisor, Kata, or Firecracker isolation, warm-pool fast starts, and an E2B-compatible API. Up and running in **60 seconds**.

<br/>

```bash
curl -sfL https://k8e.sh/install.sh | sh -
```
*That's it. Your agentic sandbox matrix is ready. 🤖*

</div>

---

## 🤖 What is K8E?

**K8E** is the **Open Source Agentic AI Sandbox Matrix** — a self-hosted sandbox platform for running secure, isolated AI agent workloads at scale, packaged as a single binary under 100MB.

As autonomous AI agents increasingly generate and execute untrusted code, robust sandboxing infrastructure is no longer optional. K8E ships everything needed to spin up a production-grade cluster in under 60 seconds, with first-class primitives for agent isolation, resource governance, and persistent execution environments — purpose-built for the AI era.

> 🔒 **One cluster. Many agents. Zero trust between them.**

### Sandbox Capabilities

| Capability | Description |
|---|---|
| 🔒 **Hardware Isolation** | Pluggable runtimes: gVisor (default), Kata Containers, Firecracker microVM |
| 🌐 **Network Policies** | Bundled Cilium enforces the standard `networking.k8s.io/v1` NetworkPolicy API by default — plus eBPF `toFQDNs` egress control per session; `allowed_hosts` enforced via `--cilium-dns-proxy` (KIP-16 M10) |
| ⚖️ **Resource Quotas** | CPU/memory caps per agent session to prevent runaway costs |
| 💾 **Persistent Workspaces** | Every session carries a PVC workspace (1Gi local-path by default); warm pods boot with their claim pre-provisioned — fast start *and* durable files |
| 🧠 **Warm Pool** | Pre-booted sandbox pods for sub-500ms session claim latency; application-layer readiness handshake, adaptive sizing, per-session background-run caps |
| 📸 **Content-Addressed Snapshots** | SHA-256 CAS layerstore with zstd compression, chunked multi-layer manifests, incremental `--base` restore, server-side registry, autosquash (KIP-16 M2) |
| 📜 **Exec Transcripts** | File-backed, windowed, offset-resumable command transcripts — `k8e-sandbox-cli log` (KIP-16 M4) |
| 📊 **Observability** | Prometheus metrics, disk-only NDJSON event stream, process topology — `events` / `ps` CLI (KIP-16 M5) |
| 🔄 **Sub-agent Reuse** | Sub-agents share the parent pod + workspace; isolated reset (KIP-16 M1) |
| 🧾 **CLI Catalog** | Machine-readable command/flag surface for SDK generation — `catalog` (KIP-16 M9) |
| 🤝 **agent-sandbox compatible** | Works with [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox) |
| 🔄 **SKILL + CLI + MCP** | AI agents (Claude Code, Codex, Pi) connect via `k8e-sandbox-cli` or the `/k8e-sandbox` skill |

📚 **Full documentation: [k8e.sh/guide](https://k8e.sh/guide/what-is-k8e.html)** — guides, CLI reference, and examples live on the official site.

---

## 🏗️ Architecture

<div align="center">

```
 AI Agents (Claude Code / Codex / Pi / dsh)
        │  k8e-sandbox-cli / plugin tools    (gRPC over mTLS)
        ▼
┌──────────────────────────────────────────────┐
│              SANDBOX GATEWAY                 │
│  sessions · exec · files · PTY terminals     │
│  expose · allow-hosts · snapshots            │
│  warm pool · metrics · event stream          │
└──────────────┬───────────────────────────────┘
               │ claims ready pods from the warm pool
   ┌───────────▼───────────┐   ┌────────────────┐
   │   SANDBOX POD         │   │   SANDBOX POD  │
   │   gVisor / Kata / FC  │ … │   (isolated)   │
   │   agent's code + PVC  │   │                │
   └───────────────────────┘   └────────────────┘
        eBPF per-session network policy between all of them
```

</div>

One gateway fronts every operation — session lifecycle, streaming exec,
filesystem, PTY terminals, service exposure (`expose`), live egress policy
(`allow-hosts`) and content-addressed snapshots — so agents get one audited
door instead of raw infrastructure access.

---

## 🚀 Quick Start

### Step 1 — Install a Sandbox Runtime (recommended: before K8E)

**gVisor is recommended** — no KVM required:

```bash
ARCH=$(uname -m)   # x86_64 on most servers, aarch64 on ARM
URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}

wget ${URL}/gvisor.tar.zstd ${URL}/gvisor.tar.zstd.sha512
sha512sum -c gvisor.tar.zstd.sha512   # must print OK
sudo tar --zstd -xf gvisor.tar.zstd -C /usr/local/bin
```

> K8E detects `runsc` at startup and automatically injects the gVisor stanza into its containerd config. Do **not** run `runsc install` — K8E manages its own containerd configuration.

> Need stronger isolation? Kata Containers and Firecracker are covered in the [Sandbox Runtime Setup guide](https://k8e.sh/guide/sandbox-runtime-setup.html).

### Step 2 — Install K8E

```bash
curl -sfL https://k8e.sh/install.sh | sh -
```

### Step 3 — Verify the Sandbox

```bash
k8e-sandbox-cli status        # -> {"available": true, ...}
k8e-sandbox-cli run 'echo hello from the sandbox'
```

### Step 4 — Connect Your AI Agent

Download the standalone sandbox CLI, authenticate, and install the `/k8e-sandbox` skill into your agent:

```bash
# 1. Download (~44MB) — pick your platform suffix
#    k8e-sandbox-cli-linux-amd64 / linux-arm64 / darwin-arm64 / windows-amd64.exe
curl -sLO https://github.com/xiaods/k8e/releases/latest/download/k8e-sandbox-cli-linux-amd64
chmod +x k8e-sandbox-cli-linux-amd64

# 2. Create an API key on the server (default TTL 30 days; --ttl never for non-expiring)
k8e sandbox-apikey create my-agent
# → {"name":"my-agent","key":"k8e-abc123...","ttl_days":30,"expires_at":"..."}

# 3. Connect: mTLS auth + install the skill into Claude Code / Codex / Pi
./k8e-sandbox-cli-linux-amd64 --endpoint <server-ip>:50051 --apikey k8e-abc123... connect
```

Then ask your agent naturally:

> "Run this Python snippet in a sandbox"

The agent executes `k8e-sandbox-cli run` automatically — no session management needed.

> 📖 **Tutorial walkthroughs, the full command reference, and configuration
> details live in the [Sandbox CLI guide](https://k8e.sh/guide/sandbox-cli.html)
> and [Sandbox Examples](https://k8e.sh/guide/sandbox-examples.html) on the
> official docs site.**

---

## 🆚 K8E vs Other Sandbox Platforms

| | K8E 🚀 | [E2B](https://e2b.dev) | [Daytona](https://daytona.io) | [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | DIY gVisor/Firecracker |
|---|---|---|---|---|---|
| Self-hosted, single binary | ✅ <100MB | ⚠️ Heavy (per-env VM images) | ✅ | ❌ needs a K8s cluster | ❌ you build it |
| Isolation runtimes | ✅ **gVisor / Kata / Firecracker** — pluggable | Firecracker microVMs | ✅ microVM/containers | K8s RuntimeClass | one runtime |
| E2B SDK compatibility | ✅ native (official SDKs unmodified) | ✅ native | ❌ own API | ❌ | ❌ |
| Warm pool (sub-500ms claims) | ✅ built-in, adaptive sizing | ✅ managed | ⚠️ | ⚠️ manual scaling | ❌ |
| Per-session network policy (eBPF) | ✅ Cilium, live-configurable | managed (fixed) | ⚠️ | ⚠️ NetworkPolicy | hand-written |
| License | Apache 2.0 | Apache 2.0 (hosted core paid) | Apache 2.0 | Apache 2.0 | — |

**When to choose K8E:** you want E2B-style sandboxes but self-hosted, with a rich tool surface beyond "run code" — PTY terminals, snapshots, service exposure, and live egress policy, all through one audited gateway.

---

## 🤝 Contributing

```bash
git clone https://github.com/<your-username>/k8e.git && cd k8e
git checkout -b feat/my-feature
make && make test
git push origin feat/my-feature
```

- 🐛 [Bug Reports](https://github.com/xiaods/k8e/issues/new)
- 💡 [Feature Requests](https://github.com/xiaods/k8e/issues/new)
- 🔍 [Open PRs](https://github.com/xiaods/k8e/pulls)

---

## 🛡️ Security

Report vulnerabilities via [GitHub Security Advisories](https://github.com/xiaods/k8e/security/advisories). Do not open public issues for security bugs.

---

## 📄 License

Apache License 2.0 — see [LICENSE](https://github.com/xiaods/k8e/blob/main/LICENSE).

---

## 🙏 Acknowledgments

| Project | Contribution |
|---|---|
| 🐄 [**K3s**](https://github.com/k3s-io/k3s) | Lightweight Kubernetes foundation that inspired K8E |
| ☸️ [**Kubernetes**](https://github.com/kubernetes/kubernetes) | The orchestration engine everything is built on |
| 🔷 [**Cilium**](https://github.com/cilium/cilium) | eBPF-powered networking and per-session egress control |
| 🤖 [**agent-sandbox**](https://github.com/kubernetes-sigs/agent-sandbox) | Kubernetes-native agent sandboxing primitives |
| 🌐 [**CNCF**](https://cncf.io) | Fostering the open-source cloud native ecosystem |

---

<div align="center">

**k8e.sh — Open Source Agentic AI Sandbox Matrix**

[![GitHub](https://img.shields.io/badge/GitHub-xiaods%2Fk8e-181717?style=for-the-badge&logo=github)](https://github.com/xiaods/k8e)
[![Website](https://img.shields.io/badge/Website-k8e.sh-00D4FF?style=for-the-badge&logo=googlechrome&logoColor=white)](https://k8e.sh)
[![Docs](https://img.shields.io/badge/Docs-k8e.sh%2Fdocs-green?style=for-the-badge&logo=gitbook&logoColor=white)](https://k8e.sh/guide/what-is-k8e.html)

*If K8E powers your agents, give us a ⭐ — it means the world to us!*

</div>
