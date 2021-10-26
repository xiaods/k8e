#!/bin/sh
set -e
set -o noglob

# ---use binary install directory
TMP_DIR=/tmp
BIN_DIR=/usr/local/bin
PROFILE=~/.bashrc
SYSTEM_NAME=k8e
# --- use systemd directory if defined or create default ---
SYSTEMD_DIR=/etc/systemd/system
# --- set related files from system name ---
SERVICE_K8E=${SYSTEM_NAME}.service
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
        $SUDO echo 'export CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock' >> "$PROFILE"
    fi
    if ! grep -s '\/usr\/local\/bin' "$PROFILE"; then
        $SUDO echo 'export PATH=$PATH:/usr/local/bin' >> ~/.bashrc
    fi
    if ! grep -s 'docker=nerdctl' "$PROFILE"; then
        $SUDO echo 'alias docker=nerdctl' >> ~/.bashrc
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

# --- download k8e and setup all-in-one functions
download_and_setup() {

    # --- use /usr/local/bin if root can write to it, otherwise use /opt/bin if it exists
    if ! $SUDO sh -c "touch ${BIN_DIR}/k8e-ro-test && rm -rf ${BIN_DIR}/k8e-ro-test"; then
        if [ -d /opt/bin ]; then
            BIN_DIR=/opt/bin
        fi
    fi
 
    info "Install... k8e binary to ${BIN_DIR}"
    cd $TMP_DIR &&
    $SUDO curl -s https://api.github.com/repos/xiaods/k8e/releases/latest \
        | grep "browser_download_url.*k8e" \
        | cut -d '"' -f 4 \
        | wget -qi - && \
         $SUDO chmod +x k8e && \
         $SUDO chmod 755 k8e && \
         $SUDO chown root:root k8e
         
    $SUDO mv $TMP_DIR/k8e $BIN_DIR/k8e

    $SUDO curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-start-bootstrap.sh -o $BIN_DIR/k8e-start-bootstrap.sh && \
    $SUDO chmod +x $BIN_DIR/k8e-start-bootstrap.sh

    $SUDO curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-start-server.sh -o $BIN_DIR/k8e-start-server.sh && \
    $SUDO  chmod +x $BIN_DIR/k8e-start-server.sh

    $SUDO curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-start-agent.sh -o $BIN_DIR/k8e-start-agent.sh &&  \
    $SUDO chmod +x $BIN_DIR/k8e-start-agent.sh

    $SUDO curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-stop.sh -o $BIN_DIR/k8e-stop.sh && \
    $SUDO  chmod +x $BIN_DIR/k8e-stop.sh

    $SUDO curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-killall.sh -o $BIN_DIR/k8e-killall.sh && \
    $SUDO   chmod +x $BIN_DIR/k8e-killall.sh

    $SUDO curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-uninstall.sh -o $BIN_DIR/k8e-uninstall.sh && \
    $SUDO  chmod +x $BIN_DIR/k8e-uninstall.sh


    create_symlinks
    source_profile
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