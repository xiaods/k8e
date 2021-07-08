#!/bin/sh
set -e
set -o noglob

echo "Install... k8e binary to /opt/k8e"

mkdir -p /opt/k8e && cd /opt/k8e

curl -s https://api.github.com/repos/xiaods/k8e/releases/latest \
| grep "browser_download_url.*k8e" \
| cut -d '"' -f 4 \
| wget -qi - &&  chmod +x k8e

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-bootstrap.sh -o start-bootstrap.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-server.sh -o start-server.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/start-agent.sh -o start-agent.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/stop-k8e.sh -o stop-k8e.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/setup-k8s-tools.sh -o setup-k8s-tools.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/k8e-uninstall.sh -o k8e-uninstall.sh

curl https://raw.githubusercontent.com/xiaods/k8e/master/contrib/setup-profile.sh -o setup-profile.sh

bash setup-k8s-tools.sh
bash setup-profile.sh

echo "Done! Happy deployment."


