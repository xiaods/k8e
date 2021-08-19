#!/bin/sh
set -e
set -o noglob

# ---use binary install directory
BIN_DIR=/usr/local/bin
PROFILE=~/.bashrc

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
    $SUDO ln -sf /var/lib/k8e/k8e/data/current/bin/nerdctl ${BIN_DIR}/nerdctl
    info "add calicoctl symlink for k8e"
    $SUDO ln -sf /var/lib/k8e/k8e/data/current/bin/calicoctl ${BIN_DIR}/calicoctl
}

# --- seutp profile ---
source_profile() {
if ! grep -s 'containerd\.sock' "$PROFILE"; then
    echo 'export CONTAINERD_ADDRESS=/run/k8e/containerd/containerd.sock' >> "$PROFILE"
fi
if ! grep -s '\/usr\/local\/bin' "$PROFILE"; then
    echo 'export PATH=$PATH:/usr/local/bin' >> ~/.bashrc
fi
if ! grep -s 'docker=nerdctl' "$PROFILE"; then
    echo 'alias docker=nerdctl' >> ~/.bashrc
fi
}

# --- download k8e and setup all-in-one functions
download_and_setup() {

info "Install... k8e binary to ${BIN_DIR}"

cd $BIN_DIR

curl -s https://api.github.com/repos/xiaods/k8e/releases/latest \
| grep "browser_download_url.*k8e" \
| cut -d '"' -f 4 \
| wget -qi - &&  chmod +x k8e

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-start-bootstrap.sh -o k8e-start-bootstrap.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-start-server.sh -o k8e-start-server.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-start-agent.sh -o k8e-start-agent.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-stop.sh -o k8e-stop.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-killall.sh -o k8e-killall.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-uninstall.sh -o k8e-uninstall.sh


create_symlinks
source_profile

$BIN_DIR/k8e check-config
info "Done! Happy deployment."

}

# --- run the install process --
{
    download_and_setup
}