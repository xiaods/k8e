# k8e end-to-end test image.
#
# The k8e binary is bind-mounted at runtime, so this image only carries the
# userland that k8e, containerd, runc, kubelet and Cilium expect to exist.
# Keep it close to a stock Ubuntu node: no systemd.
FROM ubuntu:24.04

# Set by BuildKit; fall back to the container's own architecture.
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        apparmor \
        bash \
        ca-certificates \
        conntrack \
        curl \
        ebtables \
        ethtool \
        iproute2 \
        iptables \
        iputils-ping \
        jq \
        kmod \
        less \
        libseccomp2 \
        mount \
        procps \
        socat \
        tcpdump \
        tzdata \
        util-linux \
    && rm -rf /var/lib/apt/lists/*

# containerd, its shims and runc are resolved on PATH. `zig build k8e` produces
# cmd/server, which -- unlike the release cmd/k8e multicall binary -- stages no
# bundled runtimes, so the image has to supply them for the agent profiles.
# containerd stays on the 1.7 series: k8e renders a
# plugins."io.containerd.grpc.v1.cri" config, which containerd 2.x rejects.
ENV CONTAINERD_VERSION=1.7.28
ENV RUNC_VERSION=1.5.1
# The two runtime releases are the only downloads here, so every hop -- including
# redirects -- is pinned to HTTPS. containerd and kubelet also probe /run,
# /var/lib/kubelet and /var/log/pods; creating them keeps the container usable
# even when the runtime does not pre-create them.
RUN set -eux; \
    mkdir -p /run /var/lib/kubelet /var/log/pods; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${arch}" in \
        amd64) containerd_arch=amd64; runc_arch=amd64 ;; \
        arm64) containerd_arch=arm64; runc_arch=arm64 ;; \
        *) echo "unsupported architecture: ${arch}" >&2; exit 1 ;; \
    esac; \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
        "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${containerd_arch}.tar.gz" \
        | tar -xz -C /usr/local; \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
        -o /usr/local/bin/runc \
        "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${runc_arch}"; \
    chmod 0755 /usr/local/bin/runc; \
    containerd --version; \
    runc --version

# Prepares the kubelet cgroup root (see entrypoint.sh) before k8e starts.
COPY entrypoint.sh /usr/local/bin/k8e-e2e-entrypoint
RUN chmod 0755 /usr/local/bin/k8e-e2e-entrypoint
ENTRYPOINT ["/usr/local/bin/k8e-e2e-entrypoint"]

WORKDIR /test
