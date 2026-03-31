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
| 2 | [🔒 Agentic AI Sandbox](#-agentic-ai-sandbox) |
| 3 | [✨ Why K8E?](#-why-k8e) |
| 4 | [🏗️ Architecture](#️-architecture) |
| 5 | [⚙️ Components](#️-components) |
| 6 | [🚀 Quick Start](#-quick-start) |
| 7 | [🔒 Sandbox Runtime Setup](#-sandbox-runtime-setup-optional) |
| 8 | [🖥️ Installation Guide](#️-installation-guide) |
| 9 | [🔧 Configuration](#-configuration) |
| 10 | [🆚 K8E vs Others](#-k8e-vs-the-alternatives) |
| 10 | [🤝 Contributing](#-contributing) |
| 11 | [🙏 Acknowledgments](#-acknowledgments) |

---

## 🤖 What is K8E?

**K8E (Kubernetes Easy Engine)** is an open-source, enterprise-grade Kubernetes distribution and the foundation for the **Agentic AI Sandbox Matrix** — a Kubernetes-native platform for running secure, isolated AI agent workloads at scale.

As autonomous AI agents increasingly generate and execute untrusted code, the need for robust sandboxing infrastructure becomes critical. K8E addresses this directly: it ships as a single binary with everything needed to spin up a production-grade Kubernetes cluster in under 60 seconds, with first-class primitives for agent isolation, resource governance, and ephemeral execution environments.

> 🔒 **One cluster. Many agents. Zero trust between them.**

---

## 🔒 Agentic AI Sandbox

K8E is purpose-built for the AI era. The **Agentic AI Sandbox Matrix** provides Kubernetes-native infrastructure for deploying, isolating, and governing autonomous AI agent workloads.

<div align="center">

```
┌─────────────────────────────────────────────────────────────┐
│              AGENTIC AI SANDBOX MATRIX (K8E)                │
│                                                             │
│  ┌──────────────┐    ┌──────────────────────────────────┐   │
│  │  LLM Agent   │───▶│        Sandbox Namespace         │   │
│  │  (any model) │    │                                  │   │
│  └──────────────┘    │  ┌────────────────────────────┐  │   │
│                      │  │      Isolated Pod          │  │   │
│  ┌──────────────┐    │  │  ┌──────────────────────┐  │  │   │
│  │  Tool Use    │───▶│  │  │  Untrusted Code Exec │  │  │   │
│  │  Code/Browse │    │  │  └──────────────────────┘  │  │   │
│  └──────────────┘    │  │  Network Policy            │  │   │
│                      │  │  Resource Quota            │  │   │
│  ┌──────────────┐    │  │  Kata / runc runtime       │  │   │
│  │  Orchestrator│───▶│  └────────────────────────────┘  │   │
│  │  (MCP/A2A)   │    └──────────────────────────────────┘   │
│  └──────────────┘                                           │
└─────────────────────────────────────────────────────────────┘
```

</div>

### Deploy an Agent Sandbox

```yaml
# agent-sandbox.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: agent-sandbox
  labels:
    sandbox: "true"
---
apiVersion: v1
kind: Pod
metadata:
  name: ai-agent
  namespace: agent-sandbox
spec:
  containers:
  - name: agent
    image: python:3.11-slim
    resources:
      limits:
        memory: "512Mi"
        cpu: "500m"
    securityContext:
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
  restartPolicy: Never
```

```bash
kubectl apply -f agent-sandbox.yaml
```

### Sandbox Capabilities

| Capability | Description |
|---|---|
| 🔒 **Hardware Isolation** | Kata Containers integration for VM-level agent isolation |
| 🌐 **Network Policies** | Prevent agent data exfiltration between sandboxes |
| ⚖️ **Resource Quotas** | Cap compute per agent to prevent runaway costs |
| 🗑️ **Ephemeral Workspaces** | Auto-cleanup after agent session ends |
| 🧠 **Stateful Runtimes** | Persistent identity and storage for long-running agents |
| 🤝 **agent-sandbox compatible** | Works with [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox) |
| 🔄 **MCP / A2A ready** | Orchestrate multi-agent pipelines declaratively |

---

## ✨ Why K8E?

<div align="center">

| Feature | What it means |
|---|---|
| 🤖 **Agentic Sandbox Matrix** | Native platform for secure AI agent execution |
| ⚡ **60-second setup** | Cluster running before your coffee brews |
| 📦 **Single binary < 100MB** | Download once, run anywhere |
| 🔒 **Security hardened** | Enterprise-grade policies built in |
| 🌐 **CNCF Conformant** | 100% standard Kubernetes — no vendor lock-in |
| 🏗️ **HA with embedded etcd** | Production-grade clustering out of the box |
| 🧩 **Cilium networking** | eBPF-powered, high-performance networking |
| 💻 **Multi-arch** | x86_64, ARM64, RISC-V all supported |
| 🔄 **Helm controller built-in** | GitOps-ready from day one |

</div>

---

## 🏗️ Architecture

<div align="center">

```
┌─────────────────────────────────────────────────────────────┐
│                        K8E CLUSTER                          │
│                                                             │
│   ┌─────────────────────────────────────────────────────┐   │
│   │              CONTROL PLANE (Server Node)            │   │
│   │                                                     │   │
│   │  ┌──────────────┐  ┌─────────────┐  ┌──────────┐   │   │
│   │  │  API Server  │  │  Scheduler  │  │   etcd   │   │   │
│   │  └──────────────┘  └─────────────┘  └──────────┘   │   │
│   │  ┌──────────────────┐  ┌─────────────────────────┐  │   │
│   │  │ Controller Mgr   │  │    Helm Controller       │  │   │
│   │  └──────────────────┘  └─────────────────────────┘  │   │
│   └─────────────────────────────────────────────────────┘   │
│                          │                                   │
│              ┌───────────┴────────────┐                     │
│   ┌──────────▼──────────┐  ┌──────────▼──────────┐         │
│   │   WORKER NODE 1     │  │   WORKER NODE 2     │         │
│   │  ┌───────────────┐  │  │  ┌───────────────┐  │         │
│   │  │ Agent Sandbox │  │  │  │ Agent Sandbox │  │         │
│   │  └───────────────┘  │  │  └───────────────┘  │         │
│   │  ┌───────────────┐  │  │  ┌───────────────┐  │         │
│   │  │  Containerd   │  │  │  │  Containerd   │  │         │
│   │  └───────────────┘  │  │  └───────────────┘  │         │
│   │  ┌───────────────┐  │  │  ┌───────────────┐  │         │
│   │  │ Cilium (CNI)  │  │  │  │ Cilium (CNI)  │  │         │
│   │  └───────────────┘  │  │  └───────────────┘  │         │
│   └─────────────────────┘  └─────────────────────┘         │
└─────────────────────────────────────────────────────────────┘
```

</div>

---

## ⚙️ Components

<div align="center">

| Component | Version | Purpose |
|---|---|---|
| ☸️ **Kubernetes** | v1.35.x | Core orchestration engine |
| 🔷 **Cilium** | Latest | eBPF networking & network policy enforcement |
| 📦 **Containerd** | v1.7.x | Container runtime |
| 🔑 **etcd** | v3.5.x | Distributed key-value store |
| 🌐 **CoreDNS** | v1.11.x | Cluster DNS |
| ⚓ **Helm Controller** | v0.16.x | GitOps & chart management |
| 📈 **Metrics Server** | v0.7.x | Resource metrics |
| 💾 **Local Path Provisioner** | v0.0.30 | Persistent storage |
| 🔧 **Kine** | v0.13.x | etcd shim for SQLite/MySQL |
| 🛡️ **Runc / Kata** | v1.2.x | OCI & hardware-isolated runtimes |

</div>

---

## 🚀 Quick Start

### Step 1 — Install K8E Server

```bash
curl -sfL https://k8e.sh/install.sh | sh -
```

### Step 2 — Verify Cluster

```bash
export KUBECONFIG=/etc/k8e/k8e.yaml
kubectl get nodes
```

### Step 3 — Verify Agentic AI Sandbox Matrix (auto-started)

The Sandbox Matrix starts automatically with the cluster. No extra steps needed.

```bash
# CRDs are applied automatically
kubectl get crd | grep k8e.cattle.io

# sandbox-matrix namespace and gRPC gateway are ready
kubectl -n sandbox-matrix get pods

# RuntimeClass: gvisor and kata are registered automatically
# firecracker is registered only when /dev/kvm is present on the node
kubectl get runtimeclass

# gRPC gateway is listening on 127.0.0.1:50051 (TLS)
# Cilium base deny-all NetworkPolicy is applied to warm pods
kubectl -n sandbox-matrix get ciliumnetworkpolicies
```

To disable the Sandbox Matrix:

```bash
curl -sfL https://k8e.sh/install.sh | INSTALL_K8E_EXEC="server --disable-sandbox-matrix" sh -
```

### Step 4 — Add a Worker Node (Optional)

```bash
# Get token from server
cat /var/lib/k8e/server/node-token

# On worker machine
curl -sfL https://k8e.sh/install.sh | \
  K8E_TOKEN=<token> \
  K8E_URL=https://<server-ip>:6443 \
  INSTALL_K8E_EXEC="agent" \
  sh -
```

---

## 🔒 Sandbox Runtime Setup (Optional)

The Sandbox Matrix starts automatically, but sandbox pods require a container runtime shim on each node. K8E auto-detects available runtimes and configures containerd accordingly.

### gVisor — Default (no KVM required)

```bash
# Install runsc
curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] \
  https://storage.googleapis.com/gvisor/releases release main" \
  > /etc/apt/sources.list.d/gvisor.list
apt-get update && apt-get install -y runsc
runsc install   # registers containerd shim
```

### Firecracker — Strongest isolation (requires `/dev/kvm`)

```bash
# Verify KVM
ls /dev/kvm

# Install firecracker-containerd shim + devmapper snapshotter
# See: https://github.com/firecracker-microvm/firecracker-containerd

# Prepare microVM kernel and rootfs
mkdir -p /var/lib/firecracker-containerd/runtime
# Place hello-vmlinux.bin and default-rootfs.img here
```

### Kata Containers — VM-backed fallback

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/kata-containers/kata-containers/main/utils/kata-manager.sh) install-packages"
kata-runtime check
```

After installing any runtime, restart k8e to regenerate containerd config:

```bash
systemctl restart k8e
kubectl get runtimeclass   # gvisor / firecracker / kata
```

---

## 🖥️ Installation Guide

### 🐧 Linux

```bash
# Server
curl -sfL https://k8e.sh/install.sh | sh -

# Agent
curl -sfL https://k8e.sh/install.sh | \
  K8E_TOKEN=ilovek8e \
  K8E_URL=https://<SERVER_IP>:6443 \
  INSTALL_K8E_EXEC="agent" \
  sh -
```

### 🐳 Docker / Dev Mode

```bash
docker run -d --privileged \
  -p 6443:6443 \
  --name k8e-dev \
  xiaods/k8e:latest server --cluster-init
```

### ✅ Verify

```bash
kubectl get nodes -o wide
kubectl get pods -n kube-system
cilium status
```

---

## 🔧 Configuration

### Environment Variables

```bash
# Server
K8E_TOKEN=<secret>
K8E_KUBECONFIG_OUTPUT=<path>
K8E_KUBECONFIG_MODE=644

# Agent
K8E_URL=https://<server>:6443
K8E_TOKEN=<secret>
```

### Systemd

```bash
systemctl status k8e
journalctl -u k8e -f
systemctl restart k8e
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
| Enterprise hardened | ✅ Yes | ⚠️ Partial | ✅ Yes | ⚠️ Partial |
| HA embedded etcd | ✅ Yes | ✅ Yes | ✅ Yes | ⚠️ Limited |
| CNCF conformant | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Yes |
| Multi-arch | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Yes |

</div>

---

## 🤝 Contributing

```bash
git clone https://github.com/<your-username>/k8e.git && cd k8e
git checkout -b feat/my-feature
make
make test
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
| 🔷 [**Cilium**](https://github.com/cilium/cilium) | eBPF-powered networking and security |
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
