#!/usr/bin/env bash
# K8E init-os-config — Ubuntu 24.04 LTS (Noble Numbat)
# Prepares a fresh Ubuntu server for K8E: kernel modules, sysctl, cgroups v2, containerd prereqs.
# Idempotent — safe to run multiple times.
#
# Usage: sudo bash init-os-config.sh

set -euo pipefail

log()  { echo "[$(date '+%H:%M:%S')] $*"; }
warn() { echo "[$(date '+%H:%M:%S')] ⚠  $*" >&2; }

# ── Preflight ────────────────────────────────────────────────────────────────
if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root (sudo)." >&2
  exit 1
fi

UBUNTU_VERSION=$(lsb_release -rs 2>/dev/null || echo "0")
if [[ ! "$UBUNTU_VERSION" =~ ^24\. ]]; then
  warn "This script is optimized for Ubuntu 24.04. Detected: $(lsb_release -ds 2>/dev/null || echo 'unknown')"
fi

log "K8E init-os-config — Ubuntu 24.04"

# ── 1. Disable swap ──────────────────────────────────────────────────────────
log "Disabling swap..."
swapoff -a
if grep -q 'swap' /etc/fstab; then
  sed -i '/swap/ s/^/#/' /etc/fstab
  log "  swap entries commented in /etc/fstab"
fi

# ── 2. Kernel modules ────────────────────────────────────────────────────────
log "Loading kernel modules..."
MODULES_FILE="/etc/modules-load.d/k8e.conf"
REQUIRED_MODULES=(br_netfilter overlay)

for mod in "${REQUIRED_MODULES[@]}"; do
  if modinfo "$mod" &>/dev/null; then
    modprobe "$mod" 2>/dev/null || warn "  Failed to load $mod"
  else
    warn "  Module $mod not available — install linux-modules-extra-$(uname -r)"
  fi
done

cat > "$MODULES_FILE" <<'EOF'
# K8E required kernel modules
br_netfilter
overlay
EOF
log "  modules written to $MODULES_FILE"

# ── 3. sysctl — network, fs, kernel tuning ───────────────────────────────────
log "Configuring sysctl..."
SYSCTL_FILE="/etc/sysctl.d/99-k8e.conf"

cat > "$SYSCTL_FILE" <<'EOF'
# K8E — Kubernetes + Cilium eBPF networking + sandbox runtimes
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-arptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.bridge.bridge-nf-call-iptables = 1
net.ipv4.ip_local_reserved_ports = 30000-32767

# Cilium eBPF requirements
net.core.bpf_jit_enable = 1
net.core.bpf_jit_harden = 0
net.core.bpf_jit_kallsyms = 1

# Sandbox / container scaling
vm.max_map_count = 262144
vm.swappiness = 1
fs.inotify.max_user_instances = 524288
fs.inotify.max_user_watches = 1048576
kernel.pid_max = 4194304
fs.file-max = 2097152
EOF

sysctl --system >/dev/null 2>&1
log "  sysctl applied from $SYSCTL_FILE"

# ── 4. cgroups v2 ────────────────────────────────────────────────────────────
log "Checking cgroups..."
if [[ "$(stat -fc %T /sys/fs/cgroup/ 2>/dev/null)" == "cgroup2fs" ]]; then
  log "  cgroups v2 ✓ (Ubuntu 24 default)"
else
  warn "  cgroups v1 detected. K8E works best with cgroups v2."
  warn "  Ensure systemd.unified_cgroup_hierarchy=1 in kernel cmdline if available."
fi

# ── 5. Firewall — disable conflicting firewalls (Cilium manages networking) ──
log "Disabling conflicting firewalls..."
for svc in ufw firewalld; do
  if systemctl is-active --quiet "$svc" 2>/dev/null; then
    systemctl stop "$svc" 2>/dev/null || true
    systemctl disable "$svc" 2>/dev/null || true
    log "  $svc stopped and disabled"
  fi
done

# ── 6. iptables — use legacy backend (compatible with kube-proxy if enabled) ──
log "Configuring iptables..."
for tbl in iptables ip6tables arptables ebtables; do
  if update-alternatives --list "$tbl" 2>/dev/null | grep -q legacy; then
    update-alternatives --set "$tbl" "/usr/sbin/${tbl}-legacy" >/dev/null 2>&1 || true
  fi
done
log "  iptables legacy backend configured"

# ── 7. ulimits (persistent) ──────────────────────────────────────────────────
log "Configuring ulimits..."
LIMITS_FILE="/etc/security/limits.d/99-k8e.conf"
cat > "$LIMITS_FILE" <<'EOF'
# K8E — increased limits for container and sandbox workloads
*  soft  nofile  65535
*  hard  nofile  65535
*  soft  nproc   65535
*  hard  nproc   65535
EOF

# Apply immediately for current session
ulimit -n 65535 2>/dev/null || true
ulimit -u 65535 2>/dev/null || true
log "  limits written to $LIMITS_FILE"

# ── 8. Kernel module for KVM (Firecracker/microVM support) ───────────────────
if [[ -e /dev/kvm ]]; then
  log "KVM detected ✓ (Firecracker-ready)"
else
  if grep -qE 'vmx|svm' /proc/cpuinfo; then
    warn "CPU supports virtualization but /dev/kvm not found."
    warn "  Enable VT-x/AMD-V in BIOS, or load kvm module: modprobe kvm && modprobe kvm_intel"
  else
    warn "CPU does not support hardware virtualization. Firecracker not available."
  fi
fi

# ── 9. Done ──────────────────────────────────────────────────────────────────
log "✅ OS configuration complete."
log "   Next: curl -sfL https://k8e.sh/install.sh | sh -"
