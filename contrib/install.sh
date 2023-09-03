#!/bin/sh
set -e
set -o noglob

# Example:
#   Installing a server with only etcd:
#     curl ... | INSTALL_K8E_EXEC="--disable-apiserver --disable-controller-manager --disable-scheduler" sh -
#   Installing an agent to point at a server:
#     curl ... | K8E_TOKEN=xxx K8E_URL=https://server-url:6443 sh -

# Environment variables:
#   - K8E_*
#     Environment variables which begin with K8E_ will be preserved for the
#     systemd service to use. Setting K8E_URL without explicitly setting
#     a systemd exec command will default the command to "agent", and we
#     enforce that K8E_TOKEN or K8E_CLUSTER_SECRET is also set.
#
#   - INSTALL_K8E_EXEC or script arguments
#     Command with flags to use for launching k8e in the systemd service, if
#     the command is not specified will default to "agent" if K8E_URL is set
#     or "server" if not. The final systemd command resolves to a combination
#     of EXEC and script args ($@).
#
#     The following commands result in the same behavior:
#       curl ... | INSTALL_K8E_EXEC="server --disable-etcd" sh -s -
#       curl ... | INSTALL_K8E_EXEC="server" sh -s - --disable-etcd


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

# --- add quotes to command arguments ---
quote() {
    for arg in "$@"; do
        printf '%s\n' "$arg" | sed "s/'/'\\\\''/g;1s/^/'/;\$s/\$/'/"
    done
}

# --- add indentation and trailing slash to quoted args ---
quote_indent() {
    printf ' \\\n'
    for arg in "$@"; do
        printf '\t%s \\\n' "$(quote "$arg")"
    done
}

# --- escape most punctuation characters, except quotes, forward slash, and space ---
escape() {
    printf '%s' "$@" | sed -e 's/\([][!#$%&()*;<=>?\_`{|}]\)/\\\1/g;'
}

# --- escape double quotes ---
escape_dq() {
    printf '%s' "$@" | sed -e 's/"/\\"/g'
}

# --- ensures $K8E_URL is empty or begins with https://, exiting fatally otherwise ---
verify_k8e_url() {
    case "${K8E_URL}" in
        "")
            ;;
        https://*)
            ;;
        *)
            fatal "Only https:// URLs are supported for K8E_URL (have ${K8E_URL})"
            ;;
    esac
}

# --- fatal if no systemd or openrc ---
verify_system() {
    if [ -x /bin/systemctl ] || type systemctl > /dev/null 2>&1; then
        HAS_SYSTEMD=true
        return
    fi
    fatal 'Can not find systemd or openrc to use as a process supervisor for k8e'
}

# --- define needed environment variables ---
setup_env() {
    # --- use command args if passed or create default ---
    case "$1" in
        # --- if we only have flags discover if command should be server or agent ---
        (-*|"")
            if [ -z "${K8E_URL}" ]; then
                CMD_K8E=server
            else
                if [ -z "${K8E_TOKEN}" ] && [ -z "${K8E_TOKEN_FILE}" ] && [ -z "${K8E_CLUSTER_SECRET}" ]; then
                    fatal "Defaulted k8e exec command to 'agent' because K8E_URL is defined, but K8E_TOKEN, K8E_TOKEN_FILE or K8E_CLUSTER_SECRET is not defined."
                fi
                CMD_K8E=agent
            fi
        ;;
        # --- command is provided ---
        (*)
            CMD_K8E=$1
            shift
        ;;
    esac

    verify_k8e_url

    CMD_K8E_EXEC="${CMD_K8E}$(quote_indent "$@")"

    # --- use sudo if we are not already root ---
    SUDO=sudo
    if [ $(id -u) -eq 0 ]; then
        SUDO=
    fi

    # --- use systemd type if defined or create default ---
    if [ -n "${INSTALL_K8E_TYPE}" ]; then
        SYSTEMD_TYPE=${INSTALL_K8E_TYPE}
    else
        if [ "${CMD_K8E}" = server ]; then
            SYSTEMD_TYPE=notify
        else
            SYSTEMD_TYPE=exec
        fi
    fi

    # --- use binary install directory if defined or create default ---
    if [ -n "${INSTALL_K8E_BIN_DIR}" ]; then
        BIN_DIR=${INSTALL_K8E_BIN_DIR}
    else
        # --- use /usr/local/bin if root can write to it, otherwise use /opt/bin if it exists
        BIN_DIR=/usr/local/bin
        if ! $SUDO sh -c "touch ${BIN_DIR}/k8e-ro-test && rm -rf ${BIN_DIR}/k8e-ro-test"; then
            if [ -d /opt/bin ]; then
                BIN_DIR=/opt/bin
            fi
        fi
    fi

    # --- use systemd directory if defined or create default ---
    if [ -n "${INSTALL_K8E_SYSTEMD_DIR}" ]; then
        SYSTEMD_DIR="${INSTALL_K8E_SYSTEMD_DIR}"
    else
        SYSTEMD_DIR=/etc/systemd/system
    fi

    # --- set related files from system name ---
    SERVICE_K8E=${SYSTEM_NAME}.service
    UNINSTALL_K8E_SH=${UNINSTALL_K8E_SH:-${BIN_DIR}/${SYSTEM_NAME}-uninstall.sh}
    KILLALL_K8E_SH=${KILLALL_K8E_SH:-${BIN_DIR}/k8e-killall.sh}

    # --- if bin directory is read only skip download ---
    if [ "${INSTALL_K8E_BIN_DIR_READ_ONLY}" = true ]; then
        INSTALL_K8E_SKIP_DOWNLOAD=true
    fi
}

# --- check if skip download environment variable set ---
can_skip_download_binary() {
    if [ "${INSTALL_K8E_SKIP_DOWNLOAD}" != true ] && [ "${INSTALL_K8E_SKIP_DOWNLOAD}" != binary ]; then
        return 1
    fi
}

# --- verify an executable k8e binary is installed ---
verify_k8e_is_executable() {
    if [ ! -x ${BIN_DIR}/k8e ]; then
        fatal "Executable k8e binary not found at ${BIN_DIR}/k8e"
    fi
}

# --- set arch and suffix, fatal if architecture not supported ---
setup_verify_arch() {
    if [ -z "$ARCH" ]; then
        ARCH=$(uname -m)
    fi
    case $ARCH in
        amd64)
            ARCH=amd64
            SUFFIX=
            ;;
        x86_64)
            ARCH=amd64
            SUFFIX=
            ;;
        arm64)
            ARCH=arm64
            SUFFIX=-${ARCH}
            ;;
        aarch64)
            ARCH=arm64
            SUFFIX=-${ARCH}
            ;;
        *)
            fatal "Unsupported architecture $ARCH"
    esac
}

# --- verify existence of network downloader executable ---
verify_downloader() {
    # Return failure if it doesn't exist or is no executable
    [ -x "$(command -v $1)" ] || return 1

    # Set verified executable as our downloader program and return success
    DOWNLOADER=$1
    return 0
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
    info "Create osm edge symlink for k8e"
    $SUDO ln -sf /var/lib/k8e/data/current/bin/osm ${BIN_DIR}/osm
}

# --- seutp profile ---
source_profile() {
    if ! grep -s 'containerd\.sock' "$PROFILE"; then
        $SUDO echo 'export CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock' >> $PROFILE
    fi
    if ! grep -s '/usr/local/bin' "$PROFILE"; then
        $SUDO echo 'export PATH=$PATH:/usr/local/bin' >> $PROFILE
    fi
    if ! grep -s 'KUBECONFIG=' "$PROFILE"; then
        $SUDO echo "export KUBECONFIG=/etc/${SYSTEM_NAME}/${SYSTEM_NAME}.yaml" >> $PROFILE
    fi
}

# --- disable current service if loaded --
systemd_disable() {
    $SUDO systemctl disable ${SYSTEM_NAME} >/dev/null 2>&1 || true
    $SUDO rm -f /etc/systemd/system/${SERVICE_K8E} || true
    $SUDO rm -f /etc/systemd/system/${SERVICE_K8E}.env || true
}

# --- capture current env and create file containing k8e_ variables ---
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
Description=K8E - Kubernetes Easy Engine
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
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=${BIN_DIR}/k8e \\
    ${CMD_K8E_EXEC}

EOF
}

# --- write systemd or openrc service file ---
create_service_file() {
    [ "${HAS_SYSTEMD}" = true ] && create_systemd_service_file
    return 0
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
if (ls ${SYSTEMD_DIR}/k8e*.service || ls /etc/init.d/k8e*) >/dev/null 2>&1; then
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
    $SUDO chmod 755 ${UNINSTALL_K8E_SH}
    $SUDO chown root:root ${UNINSTALL_K8E_SH}
}


# --- enable and start systemd service ---
systemd_enable() {
    info "systemd: Enabling ${SYSTEM_NAME} unit"
    $SUDO systemctl enable ${FILE_K8E_SERVICE} >/dev/null
    $SUDO systemctl daemon-reload >/dev/null
}

systemd_start() {
    info "systemd: Starting ${SYSTEM_NAME}"
    $SUDO systemctl restart ${SYSTEM_NAME}
}


# --- startup systemd or openrc service ---
service_enable_and_start() {
    [ "${INSTALL_K8E_SKIP_ENABLE}" = true ] && return
    [ "${HAS_SYSTEMD}" = true ] && systemd_enable
    [ "${INSTALL_K8E_SKIP_START}" = true ] && return

    [ "${HAS_SYSTEMD}" = true ] && systemd_start
    return 0
}

# --- install cilium network cni/operator ---
setup_cilium() {
    # waiting for k8e extract cilium binary
    sleep 1

    case "${INSTALL_K8E_EXEC}" in
        *"cluster-init"*) info "Installing cilium network cni/operator"
        $SUDO chmod 644 /etc/${SYSTEM_NAME}/${SYSTEM_NAME}.yaml
        # cilium helm values https://github.com/cilium/cilium/tree/master/install/kubernetes/cilium
        $SUDO KUBECONFIG=/etc/${SYSTEM_NAME}/${SYSTEM_NAME}.yaml $BIN_DIR/cilium install --version=1.14.1 --helm-set ipam.operator.clusterPoolIPv4PodCIDRList=["10.42.0.0/16"];;
    esac
}

# --- download and verify k8e ---
download_and_verify() {
    if can_skip_download_binary; then
       info 'Skipping k8e download and verify'
       verify_k8e_is_executable
       return
    fi

    setup_verify_arch
    verify_downloader curl || verify_downloader wget || fatal 'Can not find curl or wget for downloading files'

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
    $SUDO chown root:root "$targetFile"
    echo "Download complete."
    $SUDO mv -f "$targetFile" $BIN_DIR/$REPO
    
}

# --- check-config  ---
check_config() {
    info "init OS config && Checking k8e config"
    $SUDO $BIN_DIR/k8e init-os-config
    $SUDO $BIN_DIR/k8e check-config
}

# --- re-evaluate args to include env command ---
eval set -- $(escape "${INSTALL_K8E_EXEC}") $(quote "$@")

# --- run the install process --
{
    verify_system
    setup_env "$@"
    download_and_verify
    create_symlinks
    source_profile
    create_killall
    create_uninstall
    systemd_disable
    create_env_file
    create_service_file
    service_enable_and_start
    check_config
    setup_cilium
    info "Done! K8E - Kubernetes Easy Engine, Happy deployment."
}