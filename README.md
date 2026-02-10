<p align="center">
  <img
    src="docs/assets/k8e-logo.svg"
    alt="K8e"
    width="200"
  />
</p>

# K8E 🚀 — Instantly Ready Lightweight Kubernetes for Enterprise & AI Workloads

**K8E** (Kubernetes Easy Engine) is a lightweight, CNCF-conformant Kubernetes distribution engineered for rapid deployment and enterprise-scale operations. Built on the foundation of K3s with strategic enhancements for production environments, **K8E** delivers a fully compliant Kubernetes experience in a single binary under 100 MB—enabling clusters to be up and running in under 60 seconds.

[![Go Report Card](https://goreportcard.com/badge/github.com/xiaods/k8e)](https://goreportcard.com/report/github.com/xiaods/k8e)
[![License](https://img.shields.io/hexpm/l/apa)](https://github.com/xiaods/k8e/blob/main/LICENSE)

**Get started in 60 seconds**:  
```bash
curl -sfL https://get.k8e.sh/install.sh | K8E_TOKEN=ilovek8e INSTALL_K8E_EXEC="server --cluster-init --write-kubeconfig-mode 644" sh -
```

## Enterprise-Grade Simplicity
k8e eliminates operational complexity without compromising capabilities. It provides:
- ✅ Unified cluster lifecycle management with zero-dependency installation
- ✅ Built-in security hardening and policy enforcement for regulated environments
- ✅ Production-ready HA architecture with embedded etcd support
- ✅ Seamless integration with existing enterprise toolchains and monitoring stacks
- ✅ Minimal resource footprint ideal for edge, hybrid cloud, and cost-sensitive deployments

## Native Support for AI Agent Sandboxing
k8e is purpose-built for the AI era, offering first-class support for secure AI agent execution through Kubernetes-native sandboxing:

- **Agent Sandbox Ready**: Fully compatible with [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox), k8e enables secure, isolated execution environments for autonomous AI agents that generate and run untrusted code at scale.
- **Stateful Agent Runtimes**: Leverages k8e's optimized control plane to manage stateful, singleton agent workloads with persistent identity and storage—critical for LLM agent sessions and tool-use workflows.
- **Runtime Flexibility**: Supports secure container runtimes (including Kata Containers integration) for hardware-enforced isolation of agent execution environments
- **Declarative Orchestration**: Deploy agent sandboxes via Kubernetes Custom Resources with fine-grained resource quotas, network policies, and ephemeral workspace management

## Why K8E for AI & Enterprise?
While traditional distributions burden teams with operational overhead, K8E delivers production Kubernetes with developer-friendly simplicity—making it the ideal platform for:
- 🤖 AI/ML teams deploying agent runtimes and sandboxed inference workloads
- 🏢 Enterprises requiring certified Kubernetes with minimal footprint
- 🚀 DevOps teams seeking rapid cluster provisioning without sacrificing security

[Official Documentation](https://get.k8e.sh/docs/concepts/introduction/)

## Acknowledgments
This project is deeply inspired by and references the excellent work of the [K3s](https://github.com/k3s-io/k3s) project. We are grateful to the K3s community for their outstanding contributions to the Kubernetes ecosystem, which have made this project possible.

- Special thanks to [K3s](https://github.com/k3s-io/k3s) - The lightweight Kubernetes distribution that inspired many of the design principles and implementation approaches used in K8E.