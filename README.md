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
| 6 | [🤖 Sandbox MCP Skill](#-sandbox-mcp-skill) |
| 7 | [🐍 Python Client SDK](#-python-client-sdk) |
| 8 | [🟦 TypeScript Client SDK](#-typescript-client-sdk) |
| 9 | [🖥️ Advanced Installation](#️-advanced-installation) |
| 9 | [🆚 K8E vs Others](#-k8e-vs-the-alternatives) |
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
| 🌐 **Network Policies** | Cilium eBPF `toFQDNs` egress control — per-session, no proxy process needed |
| ⚖️ **Resource Quotas** | CPU/memory caps per agent session to prevent runaway costs |
| 🗑️ **Ephemeral Workspaces** | Auto-cleanup after agent session ends |
| 🧠 **Warm Pool** | Pre-booted sandbox pods for sub-500ms session claim latency |
| 🤝 **agent-sandbox compatible** | Works with [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox) |
| 🔄 **MCP / A2A ready** | Any MCP-compatible agent (kiro, claude, gemini) connects via `k8e sandbox-mcp` |

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
│  k8e sandbox-mcp│  ← MCP stdio bridge
└────────┬────────┘
         │  stdin/stdout
┌────────┴────────┐
│  AI Agent       │  (kiro / claude / gemini / any MCP client)
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
| 🤖 **Sandbox MCP Server** | built-in | `k8e sandbox-mcp` — agent tool bridge |

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

### Step 4 — Connect Your AI Agent

`sandbox-install-skill` does two things at once:
1. Writes the `k8e-sandbox` MCP server entry into the agent's config file
2. Copies the sandbox skill files from `/var/lib/k8e/server/skills/` into the agent's skills directory

K8E server must have started at least once before running this command (it stages the skill files on first boot).

```bash
k8e sandbox-install-skill all   # installs into kiro, claude, gemini at once
```

Then ask your agent naturally:

> "Run this Python snippet in a sandbox"

That's it. The agent calls `sandbox_run` automatically — no session management needed.

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

## 🤖 Sandbox MCP Skill

`k8e sandbox-mcp` is a built-in MCP server that bridges any MCP-compatible AI agent to K8E's sandbox infrastructure over gRPC — no extra binaries, no manual endpoint config.

```
AI Agent (kiro / claude / gemini)
    │  stdin/stdout
    ▼
k8e sandbox-mcp
    │  gRPC (TLS, auto-discovered)
    ▼
sandbox-grpc-gateway:50051
    │
    ▼
Isolated Pod (gVisor / Kata / Firecracker)
```

### Install the Skill

`sandbox-install-skill` does two things in one command:
1. Writes the `k8e-sandbox` MCP server entry into the agent's config file
2. Copies skill files from `/var/lib/k8e/server/skills/` into the agent's skills directory

> K8E server must have started at least once before running this — it stages the skill files to `/var/lib/k8e/server/skills/` on first boot.

```bash
# All supported agents at once
k8e sandbox-install-skill all

# Or per agent
k8e sandbox-install-skill kiro      # MCP config → .kiro/settings.json (workspace)
                                    # Skills     → .kiro/skills/k8e-sandbox-skill/
k8e sandbox-install-skill claude    # MCP config → ~/.claude.json
                                    # Skills     → ~/.claude/skills/k8e-sandbox-skill/
k8e sandbox-install-skill gemini    # MCP config → ~/.gemini/settings.json
                                    # Skills     → ~/.gemini/skills/k8e-sandbox-skill/
```

**Manual setup** — add to your agent's MCP config:

```json
{
  "mcpServers": {
    "k8e-sandbox": {
      "command": "k8e",
      "args": ["sandbox-mcp"]
    }
  }
}
```

For claude code:

```bash
claude mcp add k8e-sandbox -- k8e sandbox-mcp
```

### Verify

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1.0"},"capabilities":{}}}' \
  | k8e sandbox-mcp
```

### Available Tools

| Tool | Description |
|---|---|
| `sandbox_run` | Run code/commands — auto-manages full session lifecycle |
| `sandbox_status` | Check if sandbox service is available |
| `sandbox_create_session` | Create an isolated sandbox pod |
| `sandbox_destroy_session` | Destroy session and clean up |
| `sandbox_exec` | Run a command in a specific session |
| `sandbox_exec_stream` | Run a command, get streaming output |
| `sandbox_write_file` | Write a file into `/workspace` |
| `sandbox_read_file` | Read a file from `/workspace` |
| `sandbox_list_files` | List files modified since a timestamp |
| `sandbox_pip_install` | Install Python packages via pip |
| `sandbox_run_subagent` | Spawn a child sandbox (depth ≤ 1) |
| `sandbox_confirm_action` | Gate irreversible actions on user approval |

### Configuration Overrides

The MCP server auto-discovers the local cluster. Override when needed:

```bash
K8E_SANDBOX_ENDPOINT=10.0.0.1:50051 k8e sandbox-mcp          # remote cluster
K8E_SANDBOX_CERT=/path/to/ca.crt k8e sandbox-mcp              # custom TLS cert
k8e sandbox-mcp --endpoint 10.0.0.1:50051 --tls-cert /path/to/ca.crt
```

Auto-discovery probe order:
1. `K8E_SANDBOX_ENDPOINT` env var
2. `K8E_SANDBOX_CERT` / `K8E_SANDBOX_KEY` env vars
3. `/var/lib/k8e/server/tls/serving-kube-apiserver.crt` (server node, root)
4. `/etc/k8e/k8e.yaml` kubeconfig CA (agent node / non-root)
5. `127.0.0.1:50051` with system CA pool

---

## 🐍 Python Client SDK

The Python SDK talks directly to the sandbox gRPC gateway — no MCP process spawn, no stdio handshake (~1–5 ms vs ~500 ms for MCP stdio).

### Install

```bash
python3 -m pip install grpcio grpcio-tools protobuf
```

### Generate gRPC Stubs (once)

```bash
python3 -m grpc_tools.protoc -I proto \
  --python_out=sdk/python \
  --grpc_python_out=sdk/python \
  proto/sandbox/v1/sandbox.proto

# make the generated package importable
touch sdk/python/sandbox/__init__.py sdk/python/sandbox/v1/__init__.py
```

### Usage

**Run code (session auto-managed):**

```python
from sandbox_client import SandboxClient

with SandboxClient() as client:
    result = client.run("print('hello')", language="python")
    print(result.stdout)   # hello
    print(result.exit_code)  # 0
```

**Generate 10 random numbers and compute the average:**

```python
from sandbox_client import SandboxClient

code = (
    "import random; nums = [random.randint(1,100) for _ in range(10)]; "
    "print('numbers:', nums); print('average:', sum(nums)/len(nums))"
)

with SandboxClient() as client:
    result = client.run(code, language="python")
    print(result.stdout)
# numbers: [39, 60, 50, 24, 53, 32, 85, 10, 81, 3]
# average: 43.7
```

**Multi-step workflow (shared session):**

```python
with SandboxClient() as client:
    client.run("pip install pandas", "bash")   # session created
    result = client.run("python3 analyze.py", "bash")  # same session reused
```

**Explicit session with custom options:**

```python
from sandbox_client import sandbox_session

with sandbox_session(runtime_class="kata", allowed_hosts=["github.com"]) as (client, sid):
    client.write_file(sid, "/workspace/main.py", code)
    result = client.exec(sid, "python3 /workspace/main.py")
```

> SDK source: `sdk/python/sandbox_client.py`

---

## 🟦 TypeScript Client SDK

The TypeScript SDK talks directly to the sandbox gRPC gateway — no MCP process spawn, no stdio handshake (~1–5 ms vs ~500 ms for MCP stdio).

### Install

```bash
npm install @grpc/grpc-js @grpc/proto-loader
```

### Usage

**Run code (session auto-managed):**

```typescript
import { SandboxClient } from "./sandbox_client";

const client = new SandboxClient();
const result = await client.run("print('hello')", "python");
console.log(result.stdout);   // hello
await client.close();
```

**Generate 10 random numbers and compute the average:**

```typescript
const client = new SandboxClient();
const code = "import random; nums=[random.randint(1,100) for _ in range(10)]; print('numbers:',nums); print('average:',sum(nums)/len(nums))";
const result = await client.run(code, "python");
console.log(result.stdout);
// numbers: [39, 60, 50, 24, 53, 32, 85, 10, 81, 3]
// average: 43.7
await client.close();
```

**Multi-step workflow (shared session):**

```typescript
const client = new SandboxClient();
await client.run("pip install pandas", "bash");   // session created
const result = await client.run("python3 analyze.py", "bash");  // same session reused
await client.close();
```

**Explicit session with custom options:**

```typescript
const sid = await client.createSession({ runtimeClass: "kata", allowedHosts: ["github.com"] });
await client.writeFile(sid, "/workspace/main.py", code);
const result = await client.exec(sid, "python3 /workspace/main.py");
await client.destroySession(sid);
```

**Streaming output:**

```typescript
for await (const chunk of client.execStream(sid, "python3 train.py")) {
  process.stdout.write(chunk);
}
```

**One-shot helper:**

```typescript
import { sandboxRun } from "./sandbox_client";
const { stdout } = await sandboxRun("echo hello");
```

> SDK source: `sdk/typescript/sandbox_client.ts`

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
| MCP skill built-in | ✅ Yes | ❌ No | ❌ No | ❌ No |
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
