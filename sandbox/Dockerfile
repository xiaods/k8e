# syntax=docker/dockerfile:1
# k8e-sandbox: PID 1 is sandboxd (Zig static binary), runs inside gVisor/Firecracker/Kata.
# Root inside the sandbox is safe — isolation is enforced by the RuntimeClass boundary.

ARG TARGETARCH=amd64

# ── Stage 1: pick the correct sandboxd binary for this arch ──────────────────
FROM busybox:musl AS picker
ARG TARGETARCH
COPY bin/sandboxd-x86_64-linux-musl   /bins/sandboxd-amd64
COPY bin/sandboxd-aarch64-linux-musl  /bins/sandboxd-arm64
COPY bin/sandboxd-riscv64-linux-musl  /bins/sandboxd-riscv64
RUN cp /bins/sandboxd-${TARGETARCH} /sandboxd && chmod +x /sandboxd

# ── Stage 2: runtime image ───────────────────────────────────────────────────
FROM python:3.11-slim

# Basic tools agents commonly need: git, curl, bash, node/npm
RUN apt-get update && apt-get install -y --no-install-recommends \
        bash curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node.js via NodeSource (LTS) — apt nodejs is too old for most agent workloads
RUN curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Upgrade pip
RUN pip install --no-cache-dir --upgrade pip

# /workspace is the shared agent filesystem (PVC-mounted at runtime)
RUN mkdir -p /workspace
WORKDIR /workspace

# Copy the arch-matched static sandboxd binary from stage 1
COPY --from=picker /sandboxd /sandboxd

EXPOSE 2024

# sandboxd is PID 1: handles zombie reaping and signal forwarding
ENTRYPOINT ["/sandboxd"]
