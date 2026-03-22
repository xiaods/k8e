<div align="center">

<!-- Animated Banner -->
<img src="https://capsule-render.vercel.app/api?type=waving&color=0:0f2027,50:203a43,100:2c5364&height=200&section=header&text=K8E%20🚀&fontSize=80&fontColor=ffffff&fontAlignY=38&desc=Kubernetes%20Easy%20Engine&descAlignY=60&descSize=22&animation=fadeIn" width="100%"/>

<!-- Logo -->
<img src="https://raw.githubusercontent.com/xiaods/k8e/main/docs/assets/k8e-logo.png" alt="K8E Logo" width="140"/>

<br/>

<!-- Animated Typing -->
<a href="https://git.io/typing-svg">
  <img src="https://readme-typing-svg.demolab.com?font=Fira+Code&size=22&pause=1000&color=00D4FF&center=true&vCenter=true&width=700&lines=Lightweight+Kubernetes+for+Everyone+%F0%9F%8C%8D;Up+and+Running+in+60+Seconds+%E2%9A%A1;Built+for+Enterprise+%26+AI+Workloads+%F0%9F%A4%96;Single+Binary+%3C+100MB+%F0%9F%93%A6;CNCF+Conformant+%26+Production+Ready+%E2%9C%85" alt="Typing SVG" />
</a>

<br/><br/>

<!-- Badges -->
[![Go Version](https://img.shields.io/badge/Go-1.24+-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://golang.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-v1.35-326CE5?style=for-the-badge&logo=kubernetes&logoColor=white)](https://kubernetes.io)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue?style=for-the-badge&logo=apache&logoColor=white)](https://github.com/xiaods/k8e/blob/main/LICENSE)
[![Stars](https://img.shields.io/github/stars/xiaods/k8e?style=for-the-badge&logo=github&color=FFD700)](https://github.com/xiaods/k8e/stargazers)
[![Forks](https://img.shields.io/github/forks/xiaods/k8e?style=for-the-badge&logo=github&color=orange)](https://github.com/xiaods/k8e/forks)
[![Release](https://img.shields.io/github/v/release/xiaods/k8e?style=for-the-badge&logo=github&color=green)](https://github.com/xiaods/k8e/releases)
[![Build](https://img.shields.io/github/actions/workflow/status/xiaods/k8e/action-ci.yml?style=for-the-badge&logo=githubactions&logoColor=white)](https://github.com/xiaods/k8e/actions)
[![Arch](https://img.shields.io/badge/Arch-x86__64%20%7C%20ARM64%20%7C%20RISC--V-blueviolet?style=for-the-badge)](https://github.com/xiaods/k8e/releases)

<br/>

> **K8E** *(pronounced "kube-yee")* — An open-source, CNCF-conformant, enterprise-grade Kubernetes distribution in a **single binary under 100MB**. Up and running in under **60 seconds**. Built for the AI era. Inspired by [K3s](https://github.com/k3s-io/k3s).

<br/>

<!-- Quick Start Banner -->
```bash
curl -sfL https://get.k8e.sh/install.sh | K8E_TOKEN=ilovek8e INSTALL_K8E_EXEC="server --cluster-init --write-kubeconfig-mode 644" sh -
```
*That's it. Your cluster is ready. ☕*

</div>

---

## 📖 Table of Contents

| # | Section |
|---|---------|
| 1 | [🤔 What is K8E?](#-what-is-k8e) |
| 2 | [✨ Why K8E?](#-why-k8e) |
| 3 | [🏗️ Architecture](#️-architecture) |
| 4 | [⚙️ Components](#️-components) |
| 5 | [🚀 Quick Start (Beginners)](#-quick-start-for-beginners) |
| 6 | [🖥️ Installation Guide](#️-installation-guide) |
| 7 | [🤖 AI Agent Sandbox](#-ai-agent-sandbox) |
| 8 | [🔧 Configuration](#-configuration) |
| 9 | [🏢 Who Uses K8E?](#-who-uses-kubernetes--k8e) |
| 10 | [🆚 K8E vs Others](#-k8e-vs-the-alternatives) |
| 11 | [📚 Learning Resources](#-learning-resources-for-beginners) |
| 12 | [🤝 Contributing](#-contributing) |
| 13 | [🙏 Acknowledgments](#-acknowledgments) |

---

## 🤔 What is K8E?

<div align="center">
<img src="https://kubernetes.io/images/kubernetes-horizontal-color.png" width="300" alt="Kubernetes"/>
</div>

**K8E (Kubernetes Easy Engine)** is a lightweight, battle-tested Kubernetes distribution that removes all the friction from getting Kubernetes up and running — whether you're a student learning on a laptop, a startup running on a VPS, or an enterprise deploying AI agent workloads at scale.

Think of it like this:

> 🐘 **Standard Kubernetes** = A full freight train — powerful but complex to operate  
> 🚀 **K8E** = A high-speed bullet train — same power, drastically simpler to run

K8E packages the entire Kubernetes control plane — API server, scheduler, controller manager, etcd, networking (Cilium), DNS (CoreDNS), storage, and Helm — into **one single binary**. No juggling 15 different tools. No version mismatch headaches. Just one command and you're live.

---

## ✨ Why K8E?

<div align="center">

| 🟢 Feature | 💬 What it means for you |
|---|---|
| ⚡ **60-second setup** | Cluster running before your coffee brews |
| 📦 **Single binary < 100MB** | Download once, run anywhere |
| 🔒 **Security hardened** | Enterprise-grade policies built in |
| 🤖 **AI Agent Sandbox ready** | Native support for LLM agent runtimes |
| 🌐 **CNCF Conformant** | 100% standard Kubernetes — no vendor lock-in |
| 🏗️ **HA with embedded etcd** | Production-grade clustering out of the box |
| 🧩 **Cilium networking** | eBPF-powered, high-performance networking |
| 💻 **Multi-arch** | x86_64, ARM64, RISC-V all supported |
| 🔄 **Helm controller built-in** | GitOps-ready from day one |
| 📊 **Metrics server included** | Monitor your workloads immediately |

</div>

---

## 🏗️ Architecture

> **New to Kubernetes?** Here's a simple mental model before we dive in:
>
> Kubernetes is like a **hotel**. The **Control Plane** is the hotel management office — it decides where guests (your apps) go. The **Worker Nodes** are the hotel rooms — where your apps actually live and run. K8E makes both the management office and rooms incredibly easy to set up.

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
│   │                                                     │   │
│   │  ┌──────────────────┐  ┌─────────────────────────┐  │   │
│   │  │ Controller Mgr   │  │    Helm Controller       │  │   │
│   │  └──────────────────┘  └─────────────────────────┘  │   │
│   └─────────────────────────────────────────────────────┘   │
│                          │                                   │
│              ┌───────────┴────────────┐                     │
│              │                        │                     │
│   ┌──────────▼──────────┐  ┌──────────▼──────────┐         │
│   │   WORKER NODE 1     │  │   WORKER NODE 2     │         │
│   │                     │  │                     │         │
│   │  ┌───────────────┐  │  │  ┌───────────────┐  │         │
│   │  │  Your App 🐳  │  │  │  │  Your App 🐳  │  │         │
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

K8E ships with a carefully curated, tested stack of components so you never have to worry about version compatibility:

<div align="center">

| Component | Version | Purpose | Learn More |
|---|---|---|---|
| ☸️ **Kubernetes** | v1.35.x | Core orchestration engine | [docs](https://kubernetes.io/docs) |
| 🔷 **Cilium** | Latest | eBPF networking & security | [docs](https://docs.cilium.io) |
| 📦 **Containerd** | v1.7.x | Container runtime | [docs](https://containerd.io) |
| 🔑 **etcd** | v3.5.x | Distributed key-value store | [docs](https://etcd.io/docs) |
| 🌐 **CoreDNS** | v1.11.x | Cluster DNS | [docs](https://coredns.io) |
| ⚓ **Helm Controller** | v0.16.x | GitOps & chart management | [docs](https://github.com/k3s-io/helm-controller) |
| 📈 **Metrics Server** | v0.7.x | Resource metrics | [docs](https://github.com/kubernetes-sigs/metrics-server) |
| 💾 **Local Path Provisioner** | v0.0.30 | Persistent storage | [docs](https://github.com/rancher/local-path-provisioner) |
| 🔧 **Kine** | v0.13.x | etcd shim for SQLite/MySQL | [docs](https://github.com/k3s-io/kine) |
| 🛡️ **Runc** | v1.2.x | OCI container runtime | [docs](https://github.com/opencontainers/runc) |

</div>

---

## 🚀 Quick Start for Beginners

> 🧑‍🎓 **Absolute beginner?** No problem. Let's walk through this step by step.

### Step 0 — Prerequisites

Before anything else, make sure you have:

- A **Linux machine** (Ubuntu 20.04+ or similar) — physical, VM, or cloud (AWS, GCP, Azure all work)
- **2 CPU cores** and **4GB RAM** minimum
- **Linux kernel >= 4.19.57** (check with `uname -r`)
- Root or sudo access
- Open ports: `6443` (API server), `10250` (kubelet)

```bash
# Check your kernel version
uname -r

# Check your RAM
free -h

# Check your CPU
nproc
```

### Step 1 — Install K8E Server (Control Plane)

```bash
# One command. That's really it.
curl -sfL https://get.k8e.sh/install.sh | \
  K8E_TOKEN=ilovek8e \
  INSTALL_K8E_EXEC="server --cluster-init --write-kubeconfig-mode 644" \
  sh -
```

⏳ Wait about 30–60 seconds. K8E will download, install, and start automatically.

### Step 2 — Set Up kubectl Access

```bash
# Point kubectl to your new cluster
export KUBECONFIG=/etc/k8e/k8e.yaml

# Verify the cluster is running
kubectl get nodes
```

You should see something like:
```
NAME        STATUS   ROLES                  AGE   VERSION
my-server   Ready    control-plane,master   60s   v1.35.x
```

### Step 3 — Deploy Your First App

```bash
# Deploy nginx as a test
kubectl create deployment hello-k8e --image=nginx

# Expose it
kubectl expose deployment hello-k8e --port=80 --type=NodePort

# Check it's running
kubectl get pods
```

🎉 **Congratulations! You just deployed an app on Kubernetes.**

### Step 4 — Add a Worker Node (Optional)

```bash
# On your SERVER, get the node token
cat /var/lib/k8e/server/node-token

# On your WORKER machine, run:
curl -sfL https://get.k8e.sh/install.sh | \
  K8E_TOKEN=<your-token-here> \
  K8E_URL=https://<server-ip>:6443 \
  INSTALL_K8E_EXEC="agent" \
  sh -
```

---

## 🖥️ Installation Guide

### 🐧 Linux (Recommended)

```bash
# Server (Control Plane)
curl -sfL https://get.k8e.sh/install.sh | \
  K8E_TOKEN=ilovek8e \
  INSTALL_K8E_EXEC="server --cluster-init --write-kubeconfig-mode 644" \
  sh -

# Agent (Worker Node)
curl -sfL https://get.k8e.sh/install.sh | \
  K8E_TOKEN=ilovek8e \
  K8E_URL=https://<SERVER_IP>:6443 \
  INSTALL_K8E_EXEC="agent" \
  sh -
```

### 🐳 Docker / Dev Mode

```bash
# Run a quick dev cluster using Docker
docker run -d --privileged \
  -p 6443:6443 \
  --name k8e-dev \
  xiaods/k8e:latest server --cluster-init
```

### ☁️ Cloud Providers

<div align="center">

| Provider | Guide |
|---|---|
| <img src="https://img.shields.io/badge/AWS-FF9900?style=flat&logo=amazonaws&logoColor=white"/> | [AWS EC2 Setup →](https://getk8e.com/docs/concepts/introduction/) |
| <img src="https://img.shields.io/badge/GCP-4285F4?style=flat&logo=googlecloud&logoColor=white"/> | [Google Cloud Setup →](https://getk8e.com/docs/concepts/introduction/) |
| <img src="https://img.shields.io/badge/Azure-0078D4?style=flat&logo=microsoftazure&logoColor=white"/> | [Azure VM Setup →](https://getk8e.com/docs/concepts/introduction/) |
| <img src="https://img.shields.io/badge/DigitalOcean-0080FF?style=flat&logo=digitalocean&logoColor=white"/> | [DigitalOcean Droplet →](https://getk8e.com/docs/concepts/introduction/) |
| <img src="https://img.shields.io/badge/Raspberry_Pi-A22846?style=flat&logo=raspberrypi&logoColor=white"/> | [ARM / Raspberry Pi →](https://getk8e.com/docs/concepts/introduction/) |

</div>

### ✅ Verify Installation

```bash
# Check cluster health
kubectl get nodes -o wide

# Check all system pods are running
kubectl get pods -n kube-system

# Check Cilium networking status
export KUBECONFIG=/etc/k8e/k8e.yaml
cilium status
```

Expected Cilium output:
```
    /¯¯\
 /¯¯\__/¯¯\    Cilium:         OK
 \__/¯¯\__/    Operator:       OK
 /¯¯\__/¯¯\    Hubble:         disabled
 \__/¯¯\__/    ClusterMesh:    disabled
    \__/
```

---

## 🤖 AI Agent Sandbox

K8E is purpose-built for the AI era. It ships with first-class support for running **secure, isolated AI agent workloads** — perfect for LLM orchestration, autonomous agent pipelines, and sandboxed code execution.

<div align="center">

```
┌─────────────────────────────────────────────────┐
│             AI AGENT SANDBOX (K8E)              │
│                                                 │
│  ┌─────────────┐    ┌─────────────────────────┐ │
│  │  LLM Agent  │───▶│   Kubernetes Sandbox    │ │
│  │  (GPT/Claude│    │                         │ │
│  │  /Llama...) │    │  ┌─────────────────┐    │ │
│  └─────────────┘    │  │  Isolated Pod   │    │ │
│                     │  │  ┌───────────┐  │    │ │
│  ┌─────────────┐    │  │  │Untrusted  │  │    │ │
│  │  Tool Use   │───▶│  │  │Code Exec  │  │    │ │
│  │  (Code,     │    │  │  └───────────┘  │    │ │
│  │  Browser,   │    │  │  Network Policy │    │ │
│  │  Search...) │    │  │  Resource Quota │    │ │
│  └─────────────┘    │  └─────────────────┘    │ │
│                     └─────────────────────────┘ │
└─────────────────────────────────────────────────┘
```

</div>

### Deploy an AI Agent Sandbox

```yaml
# agent-sandbox.yaml
apiVersion: v1
kind: Pod
metadata:
  name: ai-agent-sandbox
  namespace: default
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

### Key AI Features

- ✅ Compatible with [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox)
- ✅ Kata Containers integration for hardware-enforced isolation
- ✅ Network policies to prevent agent data exfiltration
- ✅ Ephemeral workspaces with automatic cleanup
- ✅ Resource quotas to prevent runaway compute costs
- ✅ Stateful agent runtimes with persistent identity

---

## 🔧 Configuration

### Common Environment Variables

```bash
# Server Configuration
K8E_TOKEN=<secret>              # Cluster join token
K8E_KUBECONFIG_OUTPUT=<path>    # kubeconfig output path
K8E_KUBECONFIG_MODE=644         # kubeconfig file permissions

# Agent Configuration
K8E_URL=https://<server>:6443   # Server URL for agents
K8E_TOKEN=<secret>              # Must match server token

# Resource Customization
INSTALL_K8E_EXEC="server \
  --cluster-init \
  --disable traefik \
  --write-kubeconfig-mode 644 \
  --node-label role=master"
```

### Systemd Service

K8E runs as a systemd service out of the box:

```bash
# Check service status
systemctl status k8e

# View live logs
journalctl -u k8e -f

# Restart the service
systemctl restart k8e

# Stop the service
systemctl stop k8e
```

### Check Config

```bash
# Validate your system config before installing
sudo k8e check-config
```

---

## 🏢 Who Uses Kubernetes & K8E?

Kubernetes powers the infrastructure of the world's biggest companies. K8E makes that same technology accessible to everyone.

<div align="center">

| Company | Use Case |
|---|---|
| <img src="https://img.shields.io/badge/Google-4285F4?style=flat&logo=google&logoColor=white"/> **Google** | Runs billions of containers per week on Kubernetes (they invented it!) |
| <img src="https://img.shields.io/badge/Microsoft-0078D4?style=flat&logo=microsoft&logoColor=white"/> **Microsoft** | Azure Kubernetes Service (AKS) powers enterprise workloads globally |
| <img src="https://img.shields.io/badge/Amazon-FF9900?style=flat&logo=amazon&logoColor=white"/> **Amazon** | Amazon EKS runs Alexa, Prime Video, and AWS services |
| <img src="https://img.shields.io/badge/Spotify-1DB954?style=flat&logo=spotify&logoColor=white"/> **Spotify** | Migrated all backend services to Kubernetes for scalability |
| <img src="https://img.shields.io/badge/Airbnb-FF5A5F?style=flat&logo=airbnb&logoColor=white"/> **Airbnb** | Uses Kubernetes to handle millions of bookings per day |
| <img src="https://img.shields.io/badge/NVIDIA-76B900?style=flat&logo=nvidia&logoColor=white"/> **NVIDIA** | Runs AI training and inference workloads on Kubernetes |
| <img src="https://img.shields.io/badge/Twitter-1DA1F2?style=flat&logo=twitter&logoColor=white"/> **Twitter / X** | Scaled its entire infrastructure on Kubernetes |
| <img src="https://img.shields.io/badge/OpenAI-412991?style=flat&logo=openai&logoColor=white"/> **OpenAI** | Runs ChatGPT infrastructure on Kubernetes at massive scale |

</div>

> 💡 **K8E** brings this same enterprise-grade power to teams of any size — from a solo developer on a $5 VPS to a Fortune 500 engineering team.

---

## 🆚 K8E vs The Alternatives

<div align="center">

| Feature | K8E 🚀 | K3s | K8s (vanilla) | MicroK8s |
|---|---|---|---|---|
| Install time | **~60s** | ~90s | ~20min | ~5min |
| Binary size | **<100MB** | ~70MB | ~1GB+ | ~200MB |
| Enterprise features | ✅ Built-in | ❌ Manual | ⚠️ Complex | ❌ Limited |
| AI Sandbox support | ✅ Native | ❌ No | ⚠️ Manual | ❌ No |
| eBPF networking | ✅ Cilium | ⚠️ Optional | ⚠️ Optional | ❌ No |
| HA with embedded etcd | ✅ Yes | ✅ Yes | ✅ Yes | ⚠️ Limited |
| Multi-arch | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Yes |
| Production hardened | ✅ Yes | ⚠️ Partial | ✅ Yes | ⚠️ Partial |
| CNCF conformant | ✅ Yes | ✅ Yes | ✅ Yes | ✅ Yes |

</div>

---

## 📚 Learning Resources for Beginners

> 🧑‍💻 New to Kubernetes? Here's your complete learning path — from zero to production-ready!

### 🗺️ Learning Path

```
BEGINNER          INTERMEDIATE          ADVANCED
    │                   │                  │
    ▼                   ▼                  ▼
What is K8s?    →  Deployments      →  HA Clusters
Pods & Nodes    →  Services         →  Networking
kubectl basics  →  ConfigMaps       →  Security
Install K8E     →  Persistent Vol.  →  AI Workloads
```

### 📖 Official Documentation

| Resource | Link | Level |
|---|---|---|
| 📗 K8E Official Docs | [get.k8e.sh/docs](https://get.k8e.sh/docs/concepts/introduction/) | Beginner |
| ☸️ Kubernetes Docs | [kubernetes.io/docs](https://kubernetes.io/docs/home/) | Beginner–Advanced |
| 🎓 Kubernetes Basics Tutorial | [kubernetes.io/tutorials](https://kubernetes.io/docs/tutorials/kubernetes-basics/) | Beginner |
| 🔷 Cilium Docs | [docs.cilium.io](https://docs.cilium.io) | Intermediate |
| ⚓ Helm Docs | [helm.sh/docs](https://helm.sh/docs/) | Intermediate |
| 🌐 CNCF Landscape | [landscape.cncf.io](https://landscape.cncf.io) | All levels |

### 🎥 Video Tutorials

| Channel | Content | Link |
|---|---|---|
| 📺 TechWorld with Nana | Kubernetes Full Course | [YouTube](https://www.youtube.com/@TechWorldwithNana) |
| 📺 KodeKloud | Kubernetes Hands-on Labs | [YouTube](https://www.youtube.com/@KodeKloud) |
| 📺 Fireship | Kubernetes in 100 Seconds | [YouTube](https://www.youtube.com/@Fireship) |

### 🏋️ Hands-On Practice

| Platform | Description | Link |
|---|---|---|
| 🎮 Killercoda | Free browser-based K8s labs | [killercoda.com](https://killercoda.com/playgrounds/scenario/kubernetes) |
| 🎮 Play with K8s | Free temporary clusters | [labs.play-with-k8s.com](https://labs.play-with-k8s.com) |
| 📝 Kubernetes the Hard Way | Deep dive by Kelsey Hightower | [GitHub](https://github.com/kelseyhightower/kubernetes-the-hard-way) |

### 🔑 Essential kubectl Commands for Beginners

```bash
# 🔍 Viewing Resources
kubectl get nodes                    # List all nodes
kubectl get pods                     # List all pods
kubectl get pods -n kube-system      # List system pods
kubectl get all                      # List everything

# 🚀 Running Apps
kubectl create deployment myapp --image=nginx   # Deploy nginx
kubectl expose deployment myapp --port=80       # Expose it
kubectl scale deployment myapp --replicas=3     # Scale to 3

# 🔬 Debugging
kubectl describe pod <pod-name>      # Inspect a pod
kubectl logs <pod-name>              # View pod logs
kubectl exec -it <pod-name> -- bash  # SSH into a pod

# 🧹 Cleanup
kubectl delete deployment myapp      # Remove a deployment
kubectl delete pod <pod-name>        # Remove a pod
```

---

## 🤝 Contributing

We love contributions from the community! Whether it's fixing a typo, improving docs, or adding a feature — every bit counts.

### How to Contribute

```bash
# 1. Fork the repo on GitHub
# 2. Clone your fork
git clone https://github.com/<your-username>/k8e.git
cd k8e

# 3. Create a feature branch
git checkout -b feat/my-awesome-feature

# 4. Build from source
make

# 5. Run tests
make test

# 6. Run locally
sudo ./k8e check-config
sudo ./k8e server &
export KUBECONFIG=/etc/k8e/k8e.yaml
kubectl get nodes

# 7. Push and open a Pull Request 🎉
git push origin feat/my-awesome-feature
```

### Contribution Guidelines

- 🐛 **Bug Reports** → [Open an Issue](https://github.com/xiaods/k8e/issues/new)
- 💡 **Feature Requests** → [Open an Issue](https://github.com/xiaods/k8e/issues/new)
- 📖 **Documentation** → PRs welcome anytime!
- 🔍 **Code Review** → Check [open PRs](https://github.com/xiaods/k8e/pulls)

Please read our [contribution guidelines](https://github.com/xiaods/k8e/blob/main/CONTRIBUTING.md) before submitting a PR.

---

## 🛡️ Security

Found a security vulnerability? Please **do not** open a public issue.

Report it responsibly via [GitHub Security Advisories](https://github.com/xiaods/k8e/security/advisories) or refer to [SECURITY.md](https://github.com/xiaods/k8e/blob/main/SECURITY.md).

---

## 📄 License

K8E is open source software licensed under the [Apache License 2.0](https://github.com/xiaods/k8e/blob/main/LICENSE).

```
Copyright 2020–2026 xiaods and K8E Contributors

Licensed under the Apache License, Version 2.0
http://www.apache.org/licenses/LICENSE-2.0
```

---

## 🙏 Acknowledgments

K8E stands on the shoulders of giants. Huge thanks to:

<div align="center">

| Project | Contribution |
|---|---|
| 🐄 [**K3s**](https://github.com/k3s-io/k3s) | The lightweight Kubernetes distribution that inspired K8E's architecture |
| ☸️ [**Kubernetes**](https://github.com/kubernetes/kubernetes) | The foundation everything is built on |
| 🔷 [**Cilium**](https://github.com/cilium/cilium) | World-class eBPF networking |
| 🌐 [**CNCF**](https://cncf.io) | For fostering the open-source cloud native ecosystem |
| 📦 [**Containerd**](https://containerd.io) | Battle-tested container runtime |
| ⚓ [**Helm**](https://helm.sh) | The package manager for Kubernetes |

</div>

---

<div align="center">

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:2c5364,50:203a43,100:0f2027&height=120&section=footer&animation=fadeIn" width="100%"/>

**Built with ❤️ by the K8E community**

[![GitHub](https://img.shields.io/badge/GitHub-xiaods%2Fk8e-181717?style=for-the-badge&logo=github)](https://github.com/xiaods/k8e)
[![Website](https://img.shields.io/badge/Website-get.k8e.sh-00D4FF?style=for-the-badge&logo=googlechrome&logoColor=white)](https://get.k8e.sh)
[![Docs](https://img.shields.io/badge/Docs-getk8e.com-green?style=for-the-badge&logo=gitbook&logoColor=white)](https://getk8e.com/docs/concepts/introduction/)

*If K8E saved you time, please give us a ⭐ — it means the world to us!*

</div>
