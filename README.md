<div align="center">

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:0f2027,50:203a43,100:2c5364&height=200&section=header&text=K8E%20🚀&fontSize=80&fontColor=ffffff&fontAlignY=38&desc=Open%20Source%20Agentic%20AI%20Sandbox%20Matrix&descAlignY=60&descSize=22&animation=fadeIn" width="100%"/>
<br/>

<a href="https://git.io/typing-svg">
  <img src="https://readme-typing-svg.demolab.com?font=Fira+Code&size=22&pause=1000&color=00D4FF&center=true&vCenter=true&width=700&lines=Open+Source+Agentic+AI+Sandbox+Matrix+%F0%9F%A4%96;Secure+Isolated+Agent+Execution+at+Scale+%F0%9F%94%92;Up+and+Running+in+60+Seconds+%E2%9A%A1;Single+Binary+%3C+100MB+%F0%9F%93%A6;CNCF+Conformant+%26+Production+Ready+%E2%9C%85" alt="Typing SVG" />
</a>

<br/><br/>

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://golang.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-v1.35-326CE5?style=for-the-badge&logo=kubernetes&logoColor=white)](https://kubernetes.io)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue?style=for-the-badge&logo=apache&logoColor=white)](https://github.com/xiaods/k8e/blob/main/LICENSE)
[![Stars](https://img.shields.io/github/stars/xiaods/k8e?style=for-the-badge&logo=github&color=FFD700)](https://github.com/xiaods/k8e/stargazers)
[![Release](https://img.shields.io/github/v/release/xiaods/k8e?style=for-the-badge&logo=github&color=green)](https://github.com/xiaods/k8e/releases)
[![Arch](https://img.shields.io/badge/Arch-x86__64%20%7C%20ARM64%20%7C%20RISC--V-blueviolet?style=for-the-badge)](https://github.com/xiaods/k8e/releases)

<br/>

> **k8e.sh** — Open Source Agentic AI Sandbox Matrix. A CNCF-conformant Kubernetes distribution in a **single binary under 100MB**, purpose-built for secure, isolated AI agent execution at scale. Up and running in **60 seconds**. Inspired by [K3s](https://github.com/k3s-io/k3s).

<br/>

```bash
curl -sfL https://k8e.sh/install.sh | sh -
```
*That's it. Your agentic sandbox matrix is ready. 🤖*

</div>

---

## 📖 Table of Contents

| # | Section |
|---|---------|
| 1 | [🤖 What is K8E?](#-what-is-k8e) |
| 2 | [🏗️ Architecture](#️-architecture) |
| 3 | [⚙️ Components](#️-components) |
| 4 | [🚀 Quick Start](#-quick-start) |
| 5 | [🔒 Sandbox Runtime Setup](#-sandbox-runtime-setup) |
| 6 | [🤖 Sandbox CLI](#-sandbox-cli) |
| 7 | [🖥️ Advanced Installation](#️-advanced-installation) |
| 8 | [🆚 K8E vs Others](#-k8e-vs-the-alternatives) |
| 9 | [🤝 Contributing](#-contributing) |
| 10 | [🙏 Acknowledgments](#-acknowledgments) |

---

## 🤖 What is K8E?

**K8E** is the **Open Source Agentic AI Sandbox Matrix** — a Kubernetes-native platform for running secure, isolated AI agent workloads at scale, packaged as a single binary under 100MB.

As autonomous AI agents increasingly generate and execute untrusted code, robust sandboxing infrastructure is no longer optional. K8E ships everything needed to spin up a production-grade cluster in under 60 seconds, with first-class primitives for agent isolation, resource governance, and ephemeral execution environments — purpose-built for the AI era.

> 🔒 **One cluster. Many agents. Zero trust between them.**

### Sandbox Capabilities

| Capability | Description |
|---|---|
| 🔒 **Hardware Isolation** | Pluggable runtimes: gVisor (default), Kata Containers, Firecracker microVM |
| 🌐 **Network Policies** | Cilium eBPF `toFQDNs` egress control — per-session, no proxy process needed; `allowed_hosts` enforced via `--cilium-dns-proxy` (KIP-16 M10) |
| ⚖️ **Resource Quotas** | CPU/memory caps per agent session to prevent runaway costs |
| 🗑️ **Ephemeral Workspaces** | Auto-cleanup after agent session ends; per-session workspace isolation for sub-agents (KIP-16 M1) |
| 🧠 **Warm Pool** | Pre-booted sandbox pods for sub-500ms session claim latency; application-layer readiness handshake, adaptive sizing, per-session background-run caps |
| 📸 **Content-Addressed Snapshots** | SHA-256 CAS layerstore with zstd compression, chunked multi-layer manifests, incremental `--base` restore, server-side registry, autosquash (KIP-16 M2) |
| 📜 **Exec Transcripts** | File-backed, windowed, offset-resumable command transcripts — `k8e-sandbox-cli log` (KIP-16 M4) |
| 📊 **Observability** | Prometheus metrics, disk-only NDJSON event stream, process topology — `events` / `ps` CLI (KIP-16 M5) |
| 🔄 **Sub-agent Reuse** | Sub-agents share the parent pod + workspace; isolated reset (KIP-16 M1) |
| 🧾 **CLI Catalog** | Machine-readable command/flag surface for SDK generation — `catalog` (KIP-16 M9) |
| 🤝 **agent-sandbox compatible** | Works with [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox) |
| 🔄 **SKILL + CLI** | AI agents (claude code, codex, pi) connect via `k8e-sandbox-cli` CLI commands |

---

## 🏗️ Architecture

<div align="center">

```
┌─────────────────────────────────────────────────────────────────┐
│                          K8E CLUSTER                            │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │                CONTROL PLANE (Server Node)              │   │
│   │  ┌──────────────┐  ┌─────────────┐  ┌──────────┐       │   │
│   │  │  API Server  │  │  Scheduler  │  │   etcd   │       │   │
│   │  └──────────────┘  └─────────────┘  └──────────┘       │   │
│   │  ┌──────────────────┐  ┌──────────────────────────────┐ │   │
│   │  │  Controller Mgr  │  │  SandboxMatrix Controller    │ │   │
│   │  └──────────────────┘  └──────────────────────────────┘ │   │
│   └─────────────────────────────────────────────────────────┘   │
│                              │                                   │
│                 ┌────────────┴────────────┐                     │
│   ┌─────────────▼───────────┐  ┌──────────▼──────────────┐     │
│   │      WORKER NODE        │  │      WORKER NODE        │     │
│   │  ┌─────────────────┐    │  │  ┌─────────────────┐    │     │
│   │  │  sandbox-matrix │    │  │  │  sandbox-matrix │    │     │
│   │  │  grpc-gateway   │    │  │  │  grpc-gateway   │    │     │
│   │  │  :50051 (TLS)   │    │  │  │  :50051 (TLS)   │    │     │
│   │  └────────┬────────┘    │  │  └────────┬────────┘    │     │
│   │           │             │  │           │             │     │
│   │  ┌────────▼────────┐    │  │  ┌────────▼────────┐    │     │
│   │  │  Isolated Pods  │    │  │  │  Isolated Pods  │    │     │
│   │  │ gVisor/Kata/FC  │    │  │  │ gVisor/Kata/FC  │    │     │
│   │  └─────────────────┘    │  │  └─────────────────┘    │     │
│   │  Cilium CNI (eBPF)      │  │  Cilium CNI (eBPF)      │     │
│   └─────────────────────────┘  └─────────────────────────┘     │
└─────────────────────────────────────────────────────────────────┘
         ▲
         │  gRPC (TLS)
┌────────┴────────┐
│  k8e-sandbox-cli    │  ← CLI commands
└────────┬────────┘
         │  gRPC (TLS)
         ▼
│  AI Agent       │  (claude code / codex / pi)
└─────────────────┘
```

</div>

---

## ⚙️ Components

<div align="center">

| Component | Version | Purpose |
|---|---|---|
| ☸️ **Kubernetes** | v1.35.x | Core orchestration engine |
| 🔷 **Cilium** | Latest | eBPF networking & per-session egress policy |
| 📦 **Containerd** | v1.7.x | Container runtime |
| 🔑 **etcd** | v3.5.x | Distributed key-value store |
| 🌐 **CoreDNS** | v1.11.x | Cluster DNS |
| ⚓ **Helm Controller** | v0.16.x | GitOps & chart management |
| 📈 **Metrics Server** | v0.7.x | Resource metrics |
| 💾 **Local Path Provisioner** | v0.0.30 | Persistent storage |
| 🛡️ **gVisor / Kata / Firecracker** | — | Pluggable sandbox isolation runtimes |
| 🤖 **Sandbox CLI** | standalone | `k8e-sandbox-cli` — agent tool commands |

</div>

---

## 🚀 Quick Start

### Step 1 — Install a Sandbox Runtime (recommended: before K8E)

Install the runtime shim **before** K8E so it is auto-detected on first startup. **gVisor is recommended** — no KVM required.

```bash
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] \
  https://storage.googleapis.com/gvisor/releases release main" \
  > /etc/apt/sources.list.d/gvisor.list
apt-get update && apt-get install -y runsc
```

> K8E detects `runsc` at startup and automatically injects the gVisor stanza into its containerd config (`/var/lib/k8e/agent/etc/containerd/config.toml`). Do **not** run `runsc install` — K8E manages its own containerd configuration.

> Need stronger isolation? See [Sandbox Runtime Setup](#-sandbox-runtime-setup) for Kata Containers and Firecracker.

### Step 2 — Install K8E

```bash
curl -sfL https://k8e.sh/install.sh | sh -
```

### Step 3 — Verify Cluster

```bash
export KUBECONFIG=/etc/k8e/k8e.yaml
kubectl get nodes
kubectl get runtimeclass              # should show: gvisor
kubectl -n sandbox-matrix get pods   # Sandbox Matrix starts automatically
```

### Step 4 — Download Sandbox CLI & Connect Your AI Agent

Download the standalone sandbox CLI, authenticate, and install the skill into your agent:

```bash
# Download sandbox CLI (~44MB)
curl -sLO https://github.com/xiaods/k8e/releases/latest/download/k8e-sandbox-cli-linux-amd64
chmod +x k8e-sandbox-cli-linux-amd64

# Create an API key on the server
k8e sandbox-apikey create my-agent
# → {"name":"my-agent","key":"k8e-abc123..."}

# Connect: authenticate (mTLS) + install /k8e-sandbox skill into agent harnesses
./k8e-sandbox-cli-linux-amd64 --endpoint <server-ip>:50051 --apikey k8e-abc123... connect
```

> **Local usage:** If you're on the same machine as the K8E server, the CLI auto-discovers TLS certs — just run `k8e-sandbox-cli connect`.

Platform binaries: `k8e-sandbox-cli-{darwin,linux,windows}-{amd64,arm64}`

Then ask your agent naturally:

> "Run this Python snippet in a sandbox"

The agent executes `k8e-sandbox-cli run` automatically — no session management needed.

Supported agents: **claude code**, **codex**, **pi**.

---

## 🔒 Sandbox Runtime Setup

K8E auto-detects installed runtimes and registers the corresponding `RuntimeClass`. Choose based on your isolation requirements:

| Runtime | Isolation | Requirement | Boot time |
|---|---|---|---|
| **gVisor** | Syscall interception (userspace kernel) | None | ~10ms |
| **Kata Containers** | VM-backed (QEMU) | Nested virt or bare metal | ~500ms |
| **Firecracker** | Hardware microVM (KVM) | `/dev/kvm` | ~125ms |

### gVisor — Recommended Default

```bash
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] \
  https://storage.googleapis.com/gvisor/releases release main" \
  > /etc/apt/sources.list.d/gvisor.list
apt-get update && apt-get install -y runsc
```

> Do **not** run `runsc install` — K8E manages its own containerd config at `/var/lib/k8e/agent/etc/containerd/config.toml` and auto-injects the gVisor stanza on startup.
```

### Kata Containers

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/kata-containers/kata-containers/main/utils/kata-manager.sh) install-packages"
kata-runtime check
```

### Firecracker (requires `/dev/kvm`)

```bash
ls /dev/kvm   # verify KVM is available

# Install firecracker-containerd shim + devmapper snapshotter
# See: https://github.com/firecracker-microvm/firecracker-containerd
mkdir -p /var/lib/firecracker-containerd/runtime
# Place hello-vmlinux.bin and default-rootfs.img here
```

### Apply Changes

Install runtimes **before** starting K8E for zero-restart setup. If K8E is already running, restart it after installing a new runtime shim:

```bash
systemctl restart k8e
kubectl get runtimeclass
# NAME          HANDLER       AGE
# gvisor        runsc         10s
# kata          kata-qemu     10s
# firecracker   firecracker   10s   ← only if /dev/kvm present
```

---

## 🤖 Sandbox CLI

`k8e-sandbox-cli` is a standalone binary (~44MB) that gives AI agents direct access to K8E sandbox infrastructure — no server install needed.

```
AI Agent (claude code / codex / pi)
    │  shell command
    ▼
k8e-sandbox-cli run "print('hello')" --lang python
    │  gRPC (TLS)
    ▼
sandbox-grpc-gateway:50051
    │
    ▼
Isolated Pod (gVisor / Kata / Firecracker)
```

### Install the Skill

**On the server**, create an API key for secure remote access:

```bash
k8e sandbox-apikey create my-agent
# → {"name":"my-agent","key":"k8e-abc123..."}
```

**On the client**, download the standalone CLI, log in, and install the skill:

```bash
# 1. Download the platform-specific binary (~44MB)
curl -sLO https://github.com/xiaods/k8e/releases/latest/download/k8e-sandbox-cli-linux-amd64
chmod +x k8e-sandbox-cli-linux-amd64

# 2. Connect: mTLS auth + install /k8e-sandbox skill into Claude/Codex/Pi
#    Note: --endpoint and --apikey are global flags, placed before the subcommand
./k8e-sandbox-cli-linux-amd64 --endpoint <server-ip>:50051 --apikey k8e-abc123... connect
```

Platform binaries: `k8e-sandbox-cli-{darwin,linux,windows}-{amd64,arm64}`

Then in your agent harness:

```text
/k8e-sandbox <goal>
```

Or ask naturally: *"Run this Python snippet in a sandbox"* — the skill drives `k8e-sandbox-cli run`.

### Available Commands

| Command | Description |
|---|---|
| `k8e-sandbox-cli connect` | Connect local/remote gateway and install `/k8e-sandbox` agent skill |
| `k8e-sandbox-cli connect --skill-only` | Re-install agent skill only (no gateway dial) |
| `k8e-sandbox-cli login` | Authenticate only (mTLS cert; no skill install) |
| `k8e-sandbox-cli run <code>` | Run code or shell command (auto-creates/manages session) |
| `k8e-sandbox-cli status` | Check sandbox service availability and current session |
| `k8e-sandbox-cli create` | Create a new session (custom runtime, egress, manifest, git-repo) |
| `k8e-sandbox-cli destroy <sid>` | Destroy a session and free resources |
| `k8e-sandbox-cli write <sid> <path>` | Write file to `/workspace` (content via stdin) |
| `k8e-sandbox-cli read <sid> <path>` | Read file from `/workspace` |
| `k8e-sandbox-cli list <sid>` | List files in `/workspace` (filter by `--since` timestamp) |
| `k8e-sandbox-cli subagent <parent-sid>` | Spawn child sandbox under parent session (max depth 1) |
| `k8e-sandbox-cli confirm <sid> <action>` | Gate irreversible action on human approval |
| `k8e-sandbox-cli approve <approval-id>` | Approve a pending confirm request |
| `k8e sandbox-apikey create <name>` | Create API key for remote sandbox access (server-side) |
| `k8e sandbox-apikey list` | List API key names (server-side) |
| `k8e sandbox-apikey delete <name>` | Delete an API key (server-side) |

See [pkg/sandboxcli/skills/k8e-sandbox/SKILL.md](pkg/sandboxcli/skills/k8e-sandbox/SKILL.md) for full usage examples.

### Quick Examples

```bash
# Run Python code (auto-creates session)
k8e-sandbox-cli run "print('hello')" --lang python

# Shell command (default lang=bash)
k8e-sandbox-cli run "ls -la /workspace"

# TypeScript — type annotations run via tsx
k8e-sandbox-cli run "const nums: number[] = [1, 2, 3]; console.log(nums.reduce((a, b) => a + b, 0))" --lang ts

# Multi-line TypeScript via stdin (interfaces, async/await)
k8e-sandbox-cli run --lang ts <<'EOF'
interface User { name: string; age: number }

async function oldest(users: User[]): Promise<User> {
  return users.reduce((a, b) => (a.age > b.age ? a : b));
}

const users: User[] = [{ name: "Ada", age: 36 }, { name: "Linus", age: 54 }];
oldest(users).then((u) => console.log(`Oldest: ${u.name} (${u.age})`));
EOF

# Multi-line via stdin
k8e-sandbox-cli run --lang python <<'EOF'
for i in range(10):
    print(i)
EOF

# Default egress: pypi.org, files.pythonhosted.org, registry.npmjs.org,
#   objects.githubusercontent.com, github.com, raw.githubusercontent.com
SID=$(k8e-sandbox-cli create | jq -r .session_id)
k8e-sandbox-cli write $SID /workspace/script.py <<'PYEOF'
import pandas as pd
print(pd.__version__)
PYEOF
k8e-sandbox-cli run "pip install pandas" --session-id $SID
k8e-sandbox-cli run "python3 /workspace/script.py" --session-id $SID

# Create session with custom runtime and egress
SID=$(k8e-sandbox-cli create --runtime firecracker --allowed-hosts pypi.org,github.com | jq -r .session_id)

# Clone git repo at session creation
SID=$(k8e-sandbox-cli create --git-repo https://github.com/user/repo.git --git-ref main | jq -r .session_id)

# Stream long-running output
k8e-sandbox-cli run "python3 train.py" --session-id $SID --raw

# Tenant-based cross-process session reuse
k8e-sandbox-cli run "echo hello" --tenant my-project
```

### Configuration Overrides

The CLI auto-discovers the local cluster via TLS. For remote clusters, use `k8e-sandbox-cli login` once to set up mTLS credentials. Override when needed:

```bash
# Remote cluster: log in once (creates ~/.k8e/sandbox/{client.crt,client.key,ca.crt})
k8e-sandbox-cli --endpoint 10.0.0.1:50051 --apikey k8e-abc123... login

# After login, subsequent commands work without --apikey:
k8e-sandbox-cli run "echo hello"

# Or via environment variables:
K8E_SANDBOX_ENDPOINT=10.0.0.1:50051 K8E_SANDBOX_APIKEY=k8e-abc123... k8e-sandbox-cli login

# Override endpoint per-command:
K8E_SANDBOX_ENDPOINT=10.0.0.2:50051 k8e-sandbox-cli run "echo hello"
```

---

## 🖥️ Advanced Installation

### Add a Worker Node

```bash
# Get token from server node
cat /var/lib/k8e/server/node-token

# On worker machine
curl -sfL https://k8e.sh/install.sh | \
  K8E_TOKEN=<token> \
  K8E_URL=https://<server-ip>:6443 \
  INSTALL_K8E_EXEC="agent" \
  sh -
```

### Disable Sandbox Matrix

```bash
curl -sfL https://k8e.sh/install.sh | INSTALL_K8E_EXEC="server --disable-sandbox-matrix" sh -
```

### Key Environment Variables

```bash
K8E_TOKEN=<secret>              # cluster join token
K8E_URL=https://<server>:6443   # server URL (agent nodes)
K8E_KUBECONFIG_OUTPUT=<path>    # kubeconfig output path
```

---

## 🆚 K8E vs The Alternatives

<div align="center">

| Feature | K8E 🚀 | K3s | K8s (vanilla) | MicroK8s |
|---|---|---|---|---|
| Install time | **~60s** | ~90s | ~20min | ~5min |
| Binary size | **<100MB** | ~70MB | ~1GB+ | ~200MB |
| Agentic Sandbox | ✅ Native | ❌ No | ⚠️ Manual | ❌ No |
| eBPF networking | ✅ Cilium | ⚠️ Optional | ⚠️ Optional | ❌ No |
| Sandbox CLI standalone | ✅ Yes | ❌ No | ❌ No | ❌ No |
| HA embedded etcd | ✅ Yes | ✅ Yes | ✅ Yes | ⚠️ Limited |
| CNCF conformant | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Yes |
| Multi-arch | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Yes |

</div>

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

<div align="center">

| Project | Contribution |
|---|---|
| 🐄 [**K3s**](https://github.com/k3s-io/k3s) | Lightweight Kubernetes foundation that inspired K8E |
| ☸️ [**Kubernetes**](https://github.com/kubernetes/kubernetes) | The orchestration engine everything is built on |
| 🔷 [**Cilium**](https://github.com/cilium/cilium) | eBPF-powered networking and per-session egress control |
| 🤖 [**agent-sandbox**](https://github.com/kubernetes-sigs/agent-sandbox) | Kubernetes-native agent sandboxing primitives |
| 🌐 [**CNCF**](https://cncf.io) | Fostering the open-source cloud native ecosystem |

</div>

---

<div align="center">

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:2c5364,50:203a43,100:0f2027&height=120&section=footer&animation=fadeIn" width="100%"/>

**k8e.sh — Open Source Agentic AI Sandbox Matrix**

[![GitHub](https://img.shields.io/badge/GitHub-xiaods%2Fk8e-181717?style=for-the-badge&logo=github)](https://github.com/xiaods/k8e)
[![Website](https://img.shields.io/badge/Website-k8e.sh-00D4FF?style=for-the-badge&logo=googlechrome&logoColor=white)](https://k8e.sh)
[![Docs](https://img.shields.io/badge/Docs-k8e.sh%2Fdocs-green?style=for-the-badge&logo=gitbook&logoColor=white)](https://k8e.sh/docs/)

*If K8E powers your agents, give us a ⭐ — it means the world to us!*

</div>
