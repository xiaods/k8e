#!/bin/sh
set -e
set -o noglob

#########################
# Repo specific content #
#########################
OWNER="xiaods"
REPO="k8e"
############################
# Systemd specific content #
############################
TMP_DIR=/tmp
BIN_DIR=/usr/local/bin
PROFILE=~/.bashrc
SYSTEM_NAME=k8e
SYSTEMD_DIR=/etc/systemd/system
SERVICE_K8E=${SYSTEM_NAME}.service
UNINSTALL_K8E_SH=${UNINSTALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-uninstall.sh}
KILLALL_K8E_SH=${KILLALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-killall.sh}
FILE_K8E_SERVICE=${SYSTEMD_DIR}/${SERVICE_K8E}
FILE_K8E_ENV=${SYSTEMD_DIR}/${SERVICE_K8E}.env
SYSTEMD_TYPE=notify

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

# --- define needed environment variables ---
setup_env() {
    # --- use sudo if we are not already root ---
    SUDO=sudo
    if [ $(id -u) -eq 0 ]; then
        SUDO=
    fi

}

# --- add additional utility links ---
create_symlinks() {
    for cmd in kubectl crictl ctr; do
        if [ ! -e ${BIN_DIR}/${cmd} ]; then
            which_cmd=$(which ${cmd} 2>/dev/null || true)
            if [ -z "${which_cmd}" ]; then
                info "Creating ${BIN_DIR}/${cmd} symlink to k8e"
                $SUDO ln -sf ${BIN_DIR}/k8e ${BIN_DIR}/${cmd}
            else
                info "Skipping ${BIN_DIR}/${cmd} symlink to k8e, command exists in PATH at ${which_cmd}"
            fi
        else
            info "Skipping ${BIN_DIR}/${cmd} symlink to k8e, already exists"
        fi
    done
    info "Create nerdctl symlink for k8e"
    $SUDO ln -sf /var/lib/k8e/data/current/bin/nerdctl ${BIN_DIR}/nerdctl
    info "Create cilium ctl symlink for k8e"
    $SUDO ln -sf /var/lib/k8e/data/current/bin/cilium ${BIN_DIR}/cilium
}

# --- seutp profile ---
source_profile() {
    if ! grep -s 'containerd\.sock' "$PROFILE"; then
        $SUDO echo 'export CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock' >> $PROFILE
    fi
    if ! grep -s '\/usr\/local\/bin' "$PROFILE"; then
        $SUDO echo 'export PATH=$PATH:/usr/local/bin' >> $PROFILE
    fi
    if ! grep -s 'docker=nerdctl' "$PROFILE"; then
        $SUDO echo 'alias docker=nerdctl' >> $PROFILE
    fi
}

# --- disable current service if loaded --
systemd_disable() {
    $SUDO systemctl disable ${SYSTEM_NAME} >/dev/null 2>&1 || true
    $SUDO rm -f /etc/systemd/system/${SERVICE_K8E} || true
    $SUDO rm -f /etc/systemd/system/${SERVICE_K8E}.env || true
}

# --- capture current env and create file containing k3s_ variables ---
create_env_file() {
    info "env: Creating environment file ${FILE_K8E_ENV}"
    $SUDO touch ${FILE_K8E_ENV}
    $SUDO chmod 0600 ${FILE_K8E_ENV}
    env | grep '^K8E_' | $SUDO tee ${FILE_K8E_ENV} >/dev/null
    env | grep '^CONTAINERD_' | $SUDO tee -a ${FILE_K8E_ENV} >/dev/null
    env | grep -Ei '^(NO|HTTP|HTTPS)_PROXY' | $SUDO tee -a ${FILE_K8E_ENV} >/dev/null
}

# --- write systemd service file ---
create_systemd_service_file() {
    info "systemd: Creating service file ${FILE_K8E_SERVICE}"
    $SUDO tee ${FILE_K8E_SERVICE} >/dev/null << EOF
[Unit]
Description=Simple Kubernetes Distribution
Documentation=https://getk8e.com
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
# Having non-zero Limit*s causes performance problems due to accounting overhead
# in the kernel. We recommend using cgroups to do container-local accounting.
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=/bin/sh -xc '! /usr/bin/systemctl is-enabled --quiet nm-cloud-setup.service'
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k8e server --write-kubeconfig-mode 644
EOF
}

# --- create killall script ---
create_killall() {
    info "Creating killall script ${KILLALL_K8E_SH}"
    $SUDO tee ${KILLALL_K8E_SH} >/dev/null << \EOF
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
# Remove CNI namespaces
ip netns show 2>/dev/null | grep cni- | xargs -r -t -n 1 ip netns delete
rm -rf /var/lib/cni/
EOF
    $SUDO chmod 755 ${KILLALL_K8E_SH}
    $SUDO chown root:root ${KILLALL_K8E_SH}
}

# --- create uninstall script ---
create_uninstall() {
    info "Creating uninstall script ${UNINSTALL_K8E_SH}"
    $SUDO tee ${UNINSTALL_K8E_SH} >/dev/null << EOF
#!/bin/sh
set -x
[ \$(id -u) -eq 0 ] || exec sudo \$0 \$@
${KILLALL_K8E_SH}
if command -v systemctl; then
    systemctl disable ${SYSTEM_NAME}
    systemctl reset-failed ${SYSTEM_NAME}
    systemctl daemon-reload
fi
if command -v rc-update; then
    rc-update delete ${SYSTEM_NAME} default
fi
rm -f ${FILE_K8E_SERVICE}
rm -f ${FILE_K8E_ENV}
remove_uninstall() {
    rm -f ${UNINSTALL_K8E_SH}
}
trap remove_uninstall EXIT
if (ls ${SYSTEMD_DIR}/k3s*.service || ls /etc/init.d/k3s*) >/dev/null 2>&1; then
    set +x; echo 'Additional k3s services installed, skipping uninstall of k3s'; set -x
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
    $SUDO chmod 755 ${UNINSTALL_K8E_SH}
    $SUDO chown root:root ${UNINSTALL_K8E_SH}
}



# --- download k8e and setup all-in-one functions
download_and_setup() {

    version=""
    echo "Finding latest version from GitHub"
    version=$(curl -sI https://github.com/$OWNER/$REPO/releases/latest | grep -i "location:" | awk -F"/" '{ printf "%s", $NF }' | tr -d '\r')
    echo $version

    if [ ! $version ]; then
        echo "Failed while attempting to install $REPO. Please manually install:"
        echo ""
        echo "1. Open your web browser and go to https://github.com/$OWNER/$REPO/releases"
        echo "2. Download the latest release for your platform. Call it '$REPO'."
        echo "3. chmod +x ./$REPO"
        echo "4. mv ./$REPO $BIN_DIR"
        exit 1
    fi

    uname=$(uname)
    userid=$(id -u)

    targetFile="/tmp/$REPO"
    if [ "$userid" != "0" ]; then
        targetFile="$(pwd)/$REPO"
    fi

    if [ -e "$targetFile" ]; then
        rm "$targetFile"
    fi

    url=https://github.com/$OWNER/$REPO/releases/download/$version/$REPO
    echo "Downloading package $url as $targetFile"
    $SUDO curl -sSL $url --output "$targetFile"
    $SUDO chmod +x "$targetFile"
    echo "Download complete."
    $SUDO mv "$targetFile" $BIN_DIR/$REPO

    create_symlinks
    source_profile
    create_killall
    create_uninstall
    systemd_disable
    create_env_file
    create_systemd_service_file

    $SUDO $BIN_DIR/k8e check-config
    info "Done! Happy deployment."
}

# --- run the install process --
{
    setup_env
    download_and_setup
}