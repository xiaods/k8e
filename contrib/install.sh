#!/bin/sh
set -e

# Force HTTPS on every download (S6506): one wrapper keeps the policy in a
# single place.
https_fetch() {
    curl --proto '=https' "$@"
}

# tar(1) accepts --zstd whenever it was compiled with zstd support, but when tar
# is not linked against libzstd it merely execs an external `zstd` binary.
# `tar --zstd --help` exits 0 in that case too, so it cannot be used as a
# capability probe — probe with a real (empty) round-trip instead.
tar_can_zstd() {
    tar --zstd -cf /dev/null --files-from=/dev/null >/dev/null 2>&1
}
set -o noglob

# K8E Install Script — https://k8e.sh
#
# Usage:
#   curl -sfL https://k8e.sh/install.sh | sh -
#   curl -sfL https://k8e.sh/install.sh | K8E_TOKEN=xxx K8E_URL=https://... sh -
#
# Environment variables:
#   K8E_*              All K8E_ prefixed vars are passed to the systemd service
#   K8E_URL            Server URL (agent mode when set)
#   K8E_TOKEN          Cluster join token (default: ilovek8e)
#   INSTALL_K8E_EXEC   Override exec command (server/agent)
#   INSTALL_K8E_VERSION Specific version to install (default: latest)
#   INSTALL_K8E_BIN_DIR Binary install path (default: /usr/local/bin)
#   INSTALL_K8E_SKIP_DOWNLOAD  Skip binary download
#   INSTALL_K8E_SKIP_START     Don't start service after install

# ── Configuration ────────────────────────────────────────────────────────────
OWNER="xiaods"
REPO="k8e"
GITHUB_API="https://api.github.com/repos/${OWNER}/${REPO}"
GITHUB_DL="https://github.com/${OWNER}/${REPO}/releases/download"

TMP_DIR=${TMP_DIR:-/tmp}
BIN_DIR=${BIN_DIR:-/usr/local/bin}
SYSTEM_NAME=k8e
SYSTEMD_DIR=/etc/systemd/system
SERVICE_K8E=${SYSTEM_NAME}.service
UNINSTALL_K8E_SH=${UNINSTALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-uninstall.sh}
KILLALL_K8E_SH=${KILLALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-killall.sh}
FILE_K8E_SERVICE=${SYSTEMD_DIR}/${SERVICE_K8E}
FILE_K8E_ENV=${SYSTEMD_DIR}/${SERVICE_K8E}.env

# ── Logging ──────────────────────────────────────────────────────────────────
info()  { echo "[INFO]  $*" >&2; }
warn()  { echo "[WARN]  $*" >&2; }
fatal() { echo "[ERROR] $*" >&2; exit 1; }

# ── gVisor runtime ────────────────────────────────────────────────────────────
# Follows the official "Latest release" tarball method:
# https://gvisor.dev/docs/user_guide/install/#latest-release
# Since 2026-07 gVisor releases are multi-file bundles: runsc and
# containerd-shim-runsc-v1 plus a gvisor-bin/ directory of sidecar binaries
# that runsc looks up next to its own binary. The legacy bare-binary URLs
# (runsc, containerd-shim-runsc-v1) no longer exist.
install_gvisor() {
    # Require runsc, the containerd shim AND the gvisor-bin/ sidecar directory.
    # Legacy binary-only installs are completed, not skipped.
    if command -v runsc >/dev/null 2>&1 && command -v containerd-shim-runsc-v1 >/dev/null 2>&1 \
        && [ -d "$(dirname "$(command -v runsc)")/gvisor-bin" ]; then
        info "gVisor already installed: $(runsc --version 2>/dev/null | head -1)"
        return
    fi

    if ! command -v curl >/dev/null 2>&1 || ! command -v sha512sum >/dev/null 2>&1; then
        warn "curl/sha512sum not found, skipping gVisor install"
        return
    fi
    if ! command -v tar >/dev/null 2>&1; then
        warn "tar not found, skipping gVisor install"
        return
    fi

    info "Installing gVisor (runsc + containerd-shim-runsc-v1 + gvisor-bin sidecars)..."

    ARCH=$(uname -m)
    case "${ARCH}" in
        x86_64|amd64)      ARCH=x86_64 ;;
        aarch64|arm64)     ARCH=aarch64 ;;
        *) warn "Unsupported architecture for gVisor: ${ARCH}, skipping"; return ;;
    esac

    # gVisor's release bucket ships the bundle in both zstd and bzip2 forms.
    # Prefer zstd (~4x smaller) when the host can decode it, else fall back to bz2.
    if command -v zstd >/dev/null 2>&1 || tar_can_zstd; then
        TARBALL=gvisor.tar.zstd
    else
        TARBALL=gvisor.tar.bz2
    fi

    URL="https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}"
    GV_TMP=$(mktemp -d)
    trap 'rm -rf "${GV_TMP}"' EXIT

    for f in "${TARBALL}" "${TARBALL}.sha512"; do
        if ! https_fetch -fsSL -o "${GV_TMP}/${f}" "${URL}/${f}"; then
            warn "Failed to download ${f} from ${URL}, skipping gVisor install"
            return
        fi
    done

    if ! (cd "${GV_TMP}" && sha512sum -c "${TARBALL}.sha512"); then
        warn "gVisor checksum verification failed, skipping install"
        return
    fi

    # runsc resolves gvisor-bin/ relative to its own binary, so extract the
    # whole bundle into BIN_DIR (alongside the k8e binaries) and keep the
    # files together — do not relocate runsc alone.
    if [ "${TARBALL}" = "gvisor.tar.zstd" ] && tar_can_zstd; then
        if ! $SUDO tar --zstd -xf "${GV_TMP}/${TARBALL}" -C "${BIN_DIR}"; then
            warn "Failed to extract ${TARBALL}, skipping gVisor install"
            return
        fi
    elif [ "${TARBALL}" = "gvisor.tar.zstd" ]; then
        # tar without libzstd: decode through the zstd CLI
        if ! $SUDO zstd -dc "${GV_TMP}/${TARBALL}" | $SUDO tar -xf - -C "${BIN_DIR}"; then
            warn "Failed to extract ${TARBALL}, skipping gVisor install"
            return
        fi
    else
        if ! $SUDO tar -xjf "${GV_TMP}/${TARBALL}" -C "${BIN_DIR}"; then
            warn "Failed to extract ${TARBALL}, skipping gVisor install"
            return
        fi
    fi

    info "gVisor installed: $("${BIN_DIR}/runsc" --version 2>/dev/null | head -1)"
}

# ── Banner ───────────────────────────────────────────────────────────────────
print_logo() {
    cat >&2 <<'BANNER'
/$$        /$$$$$$           
| $$       /$$__  $$          
| $$   /$$| $$  \ $$  /$$$$$$ 
| $$  /$$/|  $$$$$$/ /$$__  $$
| $$$$$$/  >$$__  $$| $$$$$$$$
| $$_  $$ | $$  \ $$| $$_____/
| $$ \  $$|  $$$$$$/|  $$$$$$$
|__/  \__/ \______/  \_______/

K8E - Isolated sandboxes for untrusted agent code, sandbox-ready Kubernetes.
BANNER
}

# ── Helpers ──────────────────────────────────────────────────────────────────
quote() {
    for arg in "$@"; do
        printf '%s\n' "$arg" | sed "s/'/'\\\\''/g;1s/^/'/;\$s/\$/'/"
    done
}

quote_indent() {
    printf ' \\\n'
    for arg in "$@"; do
        printf '\t%s \\\n' "$(quote "$arg")"
    done
}

escape() {
    printf '%s' "$@" | sed -e 's/\([][!#$%&()*;<=>?\_`{|}]\)/\\\1/g;'
}

# ── Prerequisites ────────────────────────────────────────────────────────────
verify_system() {
    if [ -x /bin/systemctl ] || command -v systemctl >/dev/null 2>&1; then
        return 0
    fi
    fatal "systemd not found — K8E requires systemd"
}

verify_k8e_url() {
    case "${K8E_URL}" in
        ""|https://*) ;;
        *) fatal "Only https:// URLs are supported for K8E_URL (got: ${K8E_URL})" ;;
    esac
}

setup_verify_arch() {
    if [ -z "$ARCH" ]; then
        ARCH=$(uname -m)
    fi
    case $ARCH in
        amd64|x86_64) ARCH=amd64; SUFFIX="" ;;
        arm64|aarch64) ARCH=arm64; SUFFIX="-${ARCH}" ;;
        *) fatal "Unsupported architecture: $ARCH" ;;
    esac
}

verify_downloader() {
    [ -x "$(command -v "$1")" ] || return 1
    DOWNLOADER=$1
    return 0
}

# ── Environment ──────────────────────────────────────────────────────────────
setup_env() {
    case "$1" in
        -*|"")
            if [ -z "${K8E_URL}" ]; then
                CMD_K8E=server
            else
                if [ -z "${K8E_TOKEN}" ] && [ -z "${K8E_TOKEN_FILE}" ] && [ -z "${K8E_CLUSTER_SECRET}" ]; then
                    fatal "K8E_URL is set but K8E_TOKEN/K8E_TOKEN_FILE/K8E_CLUSTER_SECRET is not defined"
                fi
                CMD_K8E=agent
            fi
            ;;
        *)
            CMD_K8E=$1
            shift
            ;;
    esac

    verify_k8e_url
    CMD_K8E_EXEC="${CMD_K8E}$(quote_indent "$@")"

    # SUDO detection
    SUDO=sudo
    if [ "$(id -u)" -eq 0 ]; then
        SUDO=
    fi

    # systemd type: notify for server, exec for agent
    if [ -n "${INSTALL_K8E_TYPE}" ]; then
        SYSTEMD_TYPE=${INSTALL_K8E_TYPE}
    elif [ "${CMD_K8E}" = server ]; then
        SYSTEMD_TYPE=notify
    else
        SYSTEMD_TYPE=exec
    fi

    # Binary directory
    if [ -n "${INSTALL_K8E_BIN_DIR}" ]; then
        BIN_DIR=${INSTALL_K8E_BIN_DIR}
    elif ! $SUDO sh -c "touch ${BIN_DIR}/k8e-ro-test && rm -f ${BIN_DIR}/k8e-ro-test"; then
        if [ -d /opt/bin ]; then
            BIN_DIR=/opt/bin
        fi
    fi

    # Derived paths
    UNINSTALL_K8E_SH=${UNINSTALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-uninstall.sh}
    KILLALL_K8E_SH=${KILLALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-killall.sh}

    if [ "${INSTALL_K8E_BIN_DIR_READ_ONLY}" = true ]; then
        INSTALL_K8E_SKIP_DOWNLOAD=true
    fi
}

can_skip_download() {
    [ "${INSTALL_K8E_SKIP_DOWNLOAD}" = true ] || [ "${INSTALL_K8E_SKIP_DOWNLOAD}" = binary ]
}

verify_k8e_executable() {
    if [ ! -x "${BIN_DIR}/k8e" ]; then
        fatal "Executable k8e binary not found at ${BIN_DIR}/k8e"
    fi
}

# ── Download ─────────────────────────────────────────────────────────────────
get_latest_version() {
    if [ -n "${INSTALL_K8E_VERSION}" ]; then
        echo "${INSTALL_K8E_VERSION}"
        return
    fi
    info "Resolving latest version..."
    local tag=""
    # Use GitHub API with fallback to redirect method
    if command -v curl >/dev/null 2>&1; then
        tag=$(https_fetch -sfL --retry 3 --retry-delay 2 \
            -H "Accept: application/vnd.github+json" \
            "${GITHUB_API}/releases/latest" 2>/dev/null \
            | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
    fi
    if [ -z "$tag" ]; then
        fatal "Failed to resolve latest version. Set INSTALL_K8E_VERSION manually."
    fi
    echo "$tag"
}

download_and_verify() {
    if can_skip_download; then
        info "Skipping download (INSTALL_K8E_SKIP_DOWNLOAD)"
        verify_k8e_executable
        return
    fi

    setup_verify_arch
    verify_downloader curl || verify_downloader wget || fatal "curl or wget required for download"

    local version
    version=$(get_latest_version)
    info "Installing K8E ${version} (${ARCH})"

    local bin_name="k8e${SUFFIX}"
    local download_url="${GITHUB_DL}/${version}/${bin_name}"
    local target="${TMP_DIR}/${bin_name}"

    info "Downloading ${download_url}"
    if [ "${DOWNLOADER}" = curl ]; then
        https_fetch -sfL --retry 3 --retry-delay 2 -o "${target}" "${download_url}"
    else
        wget -q -O "${target}" "${download_url}"
    fi

    [ -s "${target}" ] || fatal "Downloaded binary is empty: ${download_url}"

    # Verify checksum if available
    local checksum_url="${GITHUB_DL}/${version}/sha256sum-${ARCH}.txt"
    if https_fetch -sfL --head "${checksum_url}" >/dev/null 2>&1; then
        info "Verifying checksum..."
        local expected=$(https_fetch -sfL "${checksum_url}" | grep "${bin_name}" | awk '{print $1}')
        local actual=$(sha256sum "${target}" | awk '{print $1}')
        if [ "${expected}" != "${actual}" ] && [ -n "${expected}" ]; then
            rm -f "${target}"
            fatal "Checksum mismatch for ${bin_name}"
        fi
    fi

    $SUDO chmod 755 "${target}"
    $SUDO chown root:root "${target}"
    $SUDO mv -f "${target}" "${BIN_DIR}/${bin_name}"

    # Create symlink without suffix for convenience
    if [ -n "${SUFFIX}" ]; then
        $SUDO ln -sf "${BIN_DIR}/${bin_name}" "${BIN_DIR}/k8e"
    fi
    info "k8e installed to ${BIN_DIR}/k8e"
}

# ── Symlinks ─────────────────────────────────────────────────────────────────
create_symlinks() {
    for cmd in kubectl crictl ctr; do
        if [ ! -e "${BIN_DIR}/${cmd}" ]; then
            if ! command -v "${cmd}" >/dev/null 2>&1; then
                info "Creating ${BIN_DIR}/${cmd} → k8e"
                $SUDO ln -sf "${BIN_DIR}/k8e" "${BIN_DIR}/${cmd}"
            else
                info "Skipping ${cmd} symlink (found in PATH)"
            fi
        fi
    done
}

# ── Profile ──────────────────────────────────────────────────────────────────
setup_profile() {
    local kubeconfig="/etc/${SYSTEM_NAME}/${SYSTEM_NAME}.yaml"
    local profile_script="/etc/profile.d/${SYSTEM_NAME}.sh"

    # System-wide profile so every new login shell inherits the environment
    # immediately after install, without requiring a manual `source`.
    info "Configuring shell environment (${profile_script})"
    $SUDO tee "${profile_script}" >/dev/null <<EOF
# K8E environment (generated by install.sh)
export CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock
export KUBECONFIG=${kubeconfig}
EOF
    $SUDO chmod 644 "${profile_script}"

    # Also update the invoking user's rc files idempotently, so non-login
    # interactive shells (e.g. gnome-terminal) pick the variables up too.
    local target_home="${HOME}"
    if [ -n "${SUDO_USER:-}" ] && [ -d "/home/${SUDO_USER}" ]; then
        target_home="/home/${SUDO_USER}"
    fi
    for rc in "${target_home}/.bashrc" "${target_home}/.zshrc"; do
        if { [ -w "$(dirname "${rc}")" ] || [ -w "${rc}" ]; } 2>/dev/null \
            && ! grep -qs "KUBECONFIG=${kubeconfig}" "${rc}" 2>/dev/null; then
            printf '\n# K8E\nexport CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock\nexport KUBECONFIG=%s\n' "${kubeconfig}" \
                | $SUDO tee -a "${rc}" >/dev/null
        fi
    done

    # Claim the environment for the remainder of this install run.
    export CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock
    export KUBECONFIG="${kubeconfig}"
}

# ── systemd ──────────────────────────────────────────────────────────────────
systemd_disable() {
    $SUDO systemctl disable ${SYSTEM_NAME} >/dev/null 2>&1 || true
    $SUDO rm -f "${FILE_K8E_SERVICE}" "${FILE_K8E_ENV}"
}

create_env_file() {
    info "Creating environment file ${FILE_K8E_ENV}"
    $SUDO touch "${FILE_K8E_ENV}"
    $SUDO chmod 0600 "${FILE_K8E_ENV}"
    env | grep '^K8E_' | $SUDO tee "${FILE_K8E_ENV}" >/dev/null
    env | grep '^CONTAINERD_' | $SUDO tee -a "${FILE_K8E_ENV}" >/dev/null

    # Proxy settings: forward what the caller exported, but never let the
    # apiserver dial its own kubelet through a proxy. Go resolves NO_PROXY on
    # every outbound request, so a shell that exported HTTP(S)_PROXY without a
    # NO_PROXY leaves the service proxying cluster-local traffic with no
    # exemption — kubectl logs/exec then fails with a proxy 502. Always append
    # the loopback and in-cluster ranges, merging with any existing value so a
    # caller who did set NO_PROXY keeps their entries.
    local no_proxy_value="${NO_PROXY:-${no_proxy:-}}"
    no_proxy_value="${no_proxy_value:+${no_proxy_value},}localhost,127.0.0.1,::1,10.42.0.0/16,10.43.0.0/16,.svc,.cluster.local,.local"
    env | grep -Ei '^(HTTP|HTTPS|ALL)_PROXY' | $SUDO tee -a "${FILE_K8E_ENV}" >/dev/null
    printf 'NO_PROXY=%s\nno_proxy=%s\n' "${no_proxy_value}" "${no_proxy_value}" \
        | $SUDO tee -a "${FILE_K8E_ENV}" >/dev/null
}

create_systemd_service_file() {
    info "Creating systemd service ${FILE_K8E_SERVICE}"
    $SUDO tee "${FILE_K8E_SERVICE}" >/dev/null << EOF
[Unit]
Description=K8E - Isolated sandboxes for untrusted agent code, sandbox-ready Kubernetes.
Documentation=https://k8e.sh
After=network-online.target
Wants=network-online.target

[Install]
WantedBy=multi-user.target

[Service]
Type=${SYSTEMD_TYPE}
EnvironmentFile=-/etc/default/%N
EnvironmentFile=-/etc/sysconfig/%N
EnvironmentFile=-${FILE_K8E_ENV}
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=${BIN_DIR}/k8e \\
    ${CMD_K8E_EXEC}

EOF
}

systemd_enable() {
    info "Enabling ${SYSTEM_NAME} service"
    $SUDO systemctl enable "${FILE_K8E_SERVICE}" >/dev/null
    $SUDO systemctl daemon-reload >/dev/null
}

systemd_start() {
    info "Starting ${SYSTEM_NAME}"
    $SUDO systemctl restart ${SYSTEM_NAME}
}

service_enable_and_start() {
    [ "${INSTALL_K8E_SKIP_ENABLE}" = true ] && return
    systemd_enable

    [ "${INSTALL_K8E_SKIP_START}" = true ] && return
    systemd_start
}

# ── Killall / Uninstall ──────────────────────────────────────────────────────
create_killall() {
    info "Creating killall script ${KILLALL_K8E_SH}"
    $SUDO tee "${KILLALL_K8E_SH}" >/dev/null << \EOF
#!/bin/sh
set -x
for service in /etc/systemd/system/k8e*.service; do
    [ -s $service ] && systemctl stop $(basename $service)
done
pschildren() {
    ps -e -o ppid= -o pid= | \
    sed -e 's/^\s*//g; s/\s\s*/\t/g;' | \
    grep -w "^$1" | \
    cut -f2
}
pstree() {
    for pid in $@; do
        echo $pid
        for child in $(pschildren $pid); do
            pstree $child
        done
    done
}
killtree() {
    kill -9 $(
        { set +x; } 2>/dev/null;
        pstree $@;
        set -x;
    ) 2>/dev/null
}
getshims() {
    ps -e -o pid= -o args= | sed -e 's/^ *//; s/\s\s*/\t/;' | grep -w 'k8e/data/[^/]*/bin/containerd-shim' | cut -f1
}
killtree $({ set +x; } 2>/dev/null; getshims; set -x)
do_unmount_and_remove() {
    set +x
    while read -r _ path _; do
        case "$path" in $1*) echo "$path" ;; esac
    done < /proc/self/mounts | sort -r | xargs -r -t -n 1 sh -c 'umount "$0" && rm -rf "$0"'
    set -x
}
do_unmount_and_remove '/run/k8e'
do_unmount_and_remove '/var/lib/k8e'
do_unmount_and_remove '/var/lib/kubelet/pods'
do_unmount_and_remove '/var/lib/kubelet/plugins'
do_unmount_and_remove '/run/netns/cni-'
ip netns show 2>/dev/null | grep cni- | xargs -r -t -n 1 ip netns delete
rm -rf /var/lib/cni/
EOF
    $SUDO chmod 755 "${KILLALL_K8E_SH}"
    $SUDO chown root:root "${KILLALL_K8E_SH}"
}

create_uninstall() {
    info "Creating uninstall script ${UNINSTALL_K8E_SH}"
    $SUDO tee "${UNINSTALL_K8E_SH}" >/dev/null << EOF
#!/bin/sh
set -x
[ \$(id -u) -eq 0 ] || exec sudo \$0 \$@
${KILLALL_K8E_SH}
if command -v systemctl; then
    systemctl disable ${SYSTEM_NAME}
    systemctl reset-failed ${SYSTEM_NAME}
    systemctl daemon-reload
fi
rm -f ${FILE_K8E_SERVICE}
rm -f ${FILE_K8E_ENV}
remove_uninstall() {
    rm -f ${UNINSTALL_K8E_SH}
}
trap remove_uninstall EXIT
if (ls ${SYSTEMD_DIR}/k8e*.service) >/dev/null 2>&1; then
    set +x; echo 'Additional k8e services installed, skipping uninstall of k8e'; set -x
    exit
fi
for cmd in kubectl crictl ctr; do
    if [ -L ${BIN_DIR}/\$cmd ]; then
        rm -f ${BIN_DIR}/\$cmd
    fi
done
rm -rf /etc/k8e
rm -rf /run/k8e
rm -rf /var/lib/k8e
rm -rf /var/lib/kubelet
rm -f ${BIN_DIR}/k8e
rm -f ${KILLALL_K8E_SH}
EOF
    $SUDO chmod 755 "${UNINSTALL_K8E_SH}"
    $SUDO chown root:root "${UNINSTALL_K8E_SH}"
}

# ── Check config ─────────────────────────────────────────────────────────────
check_config() {
    info "Initializing OS configuration..."
    if [ -x "${BIN_DIR}/k8e" ]; then
        $SUDO "${BIN_DIR}/k8e" init-os-config
        $SUDO "${BIN_DIR}/k8e" check-config
    else
        warn "k8e binary not yet available, skipping init-os-config"
    fi
}

# ── Auto-configure one-click install ─────────────────────────────────────────
auto_configure() {
    if [ -n "${INSTALL_K8E_EXEC}" ] || [ -n "${K8E_URL}" ] || [ "$#" -gt 0 ]; then
        return
    fi

    info "Auto-configuring K8E server for one-click install"

    # Generate random token if not set
    if [ -z "${K8E_TOKEN}" ]; then
        K8E_TOKEN="ilovek8e"
    fi
    export K8E_TOKEN

    # Default server command for one-click install
    export INSTALL_K8E_EXEC="server --write-kubeconfig-mode 644"
}

# ── Main ─────────────────────────────────────────────────────────────────────
print_logo
auto_configure "$@"
eval set -- $(escape "${INSTALL_K8E_EXEC}") $(quote "$@")

{
    verify_system
    setup_env "$@"
    download_and_verify
    create_symlinks
    install_gvisor
    setup_profile
    create_killall
    create_uninstall
    systemd_disable
    create_env_file
    create_systemd_service_file
    service_enable_and_start
    check_config

    # ── Print summary ─────────────────────────────────────────────────────
    echo ""
    echo "============================================"
    echo "  K8E installation complete!"
    echo "============================================"
    echo ""
    echo "  Kubeconfig:   /etc/k8e/k8e.yaml"
    echo "  Env:          source /etc/profile.d/k8e.sh   (auto-loaded on next login)"
    echo "  Token:        ${K8E_TOKEN:-ilovek8e}"
    echo "  Sandbox:      ${K8E_SANDBOX_ENDPOINT:-127.0.0.1:50051}"
    if command -v runsc >/dev/null 2>&1; then
        echo "  gVisor:       $(runsc --version 2>/dev/null | head -1)"
    else
        echo "  gVisor:       not installed — sandbox pods will stay Pending."
        echo "                Install it manually: https://gvisor.dev/docs/user_guide/install/"
    fi
    echo ""
    echo "  Verify:"
    echo "    export KUBECONFIG=/etc/k8e/k8e.yaml"
    echo "    kubectl get nodes"
    echo ""
    echo "  Join agent:"
    echo "    curl -sfL https://k8e.sh/install.sh | K8E_URL=https://<server-ip>:6443 K8E_TOKEN=${K8E_TOKEN:-ilovek8e} sh -"
    echo ""
    echo "  Sandbox CLI (on server -- auto-discovery, no login needed):"
    echo "    curl -sLO https://github.com/xiaods/k8e/releases/latest/download/k8e-sandbox-cli-\$(uname -s | tr A-Z a-z)-\$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
    echo "    chmod +x k8e-sandbox-cli-*"
    echo ""
    echo "    ./k8e-sandbox-cli-* connect --agent all"
    echo ""
    echo "  Sandbox CLI (remote client -- log in once):"
    echo "    # Create API key on server first: k8e sandbox-apikey create my-agent"
    echo "    ./k8e-sandbox-cli-* login --endpoint <server-ip>:50051 --apikey <key>"
    echo "    ./k8e-sandbox-cli-* connect --skill-only --agent all"
    echo ""
    echo "============================================"
}

# When run as `curl ... | sh -`, the installer cannot export variables into
# the caller's shell. Drop into a login shell that sources /etc/profile.d so
# the K8E environment is usable immediately.
if [ -t 1 ] && [ -c /dev/tty ] && [ -z "${INSTALL_K8E_SKIP_SHELL:-}" ]; then
    info "Starting a new shell with the K8E environment..."
    exec "${SHELL:-/bin/sh}" -l </dev/tty
fi

