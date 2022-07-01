#!/bin/sh
set -e
set -o noglob

# fix Ubuntu 20.04 systemd bug for rp_filter sysctl setting

# --- helper functions for logs ---
info()
{
    echo '[INFO] ' "$@"
}
warn()
{
    echo '[WARN] ' "$@" >&2
}
fatal()
{
    echo '[ERROR] ' "$@" >&2
    exit 1
}


# --- sysctl fix for rp_filter ---
do_cilium_sysctlfix() {
    info "Fixing rp_filter sysctl setting for Cilium on Ubuntu 20.04"
    if [ -f /etc/sysctl.d/99-zzz-override_cilium.conf ]; then
        warn "File /etc/sysctl.d/99-zzz-override_cilium.conf already exists"
        return
    fi
    cat > /etc/sysctl.d/99-zzz-override_cilium.conf <<EOF
# Disable rp_filter on Cilium interfaces since it may cause mangled packets to be dropped
net.ipv4.conf.lxc*.rp_filter = 0
net.ipv4.conf.cilium_*.rp_filter = 0
# The kernel uses max(conf.all, conf.{dev}) as its value, so we need to set .all. to 0 as well.
# Otherwise it will overrule the device specific settings.
net.ipv4.conf.all.rp_filter = 0
EOF

sudo systemctl restart systemd-sysctl
info "Done fixing rp_filter sysctl setting"
}


# --- run the install process --
{
    do_cilium_sysctlfix
}
