#!/bin/sh
set -x

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


rm -rf /etc/k8e/k8e
rm -rf /run/k8e
rm -rf /run/flannel
rm -rf /var/lib/k8e/k8e
rm -rf /var/lib/kubelet

BIN_DIR=/usr/local/bin

for cmd in kubectl crictl ctr; do
    if [ -L ${BIN_DIR}/\$cmd ]; then
        rm -f ${BIN_DIR}/\$cmd
    fi
done

info "Uninstall k8e kubernetes distribution done! welcome feedback: https://github.com/xiaods/k8e/issues"
