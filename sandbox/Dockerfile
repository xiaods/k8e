ARG TARGETARCH=amd64

FROM python:3.11-slim AS base

# Map Docker TARGETARCH to sandboxd binary suffix
ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      amd64)   echo "x86_64-linux-musl"   > /arch ;; \
      arm64)   echo "aarch64-linux-musl"  > /arch ;; \
      riscv64) echo "riscv64-linux-musl"  > /arch ;; \
      *)        echo "x86_64-linux-musl"   > /arch ;; \
    esac

COPY bin/sandboxd-* /tmp/sandboxd-bins/
RUN ARCH=$(cat /arch) && cp /tmp/sandboxd-bins/sandboxd-${ARCH} /sandboxd && chmod +x /sandboxd

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl git nodejs npm \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /workspace
WORKDIR /workspace

EXPOSE 2024
ENTRYPOINT ["/sandboxd"]
