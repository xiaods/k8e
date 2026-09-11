#!/usr/bin/env bash
set -euo pipefail

container_name="${K8E_MCP_CONTAINER:-k8e-mcp-recovery-init}"
host_port="${K8E_MCP_API_PORT:-16444}"
data_dir="${K8E_MCP_DATA_DIR:-/tmp/k8e-mcp-cluster-init}"
binary="${K8E_BINARY:-/tmp/k8e-mcp-server}"
image="${K8E_MCP_IMAGE:-golang:1.25.9}"

command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required" >&2; exit 2; }
[[ -x "$binary" ]] || { echo "missing executable K8E_BINARY=$binary" >&2; exit 2; }
cd "$(dirname "$0")/.."
mkdir -p "$data_dir"

docker run -d --name "$container_name" \
  -p "127.0.0.1:${host_port}:6443" \
  -v "$data_dir:/test" \
  -v "$binary:/usr/local/bin/k8e:ro" \
  "$image" \
  k8e server --cluster-init \
    --data-dir /test/data \
    --write-kubeconfig /test/kubeconfig.yaml \
    --https-listen-port 6443 \
    --tls-san 127.0.0.1 \
    --disable-agent --disable-sandbox-matrix --disable-e2b \
    --disable-cilium --disable-cloud-controller \
    --egress-selector-mode disabled >/dev/null

kubeconfig="$data_dir/client-kubeconfig.yaml"
for _ in $(seq 1 120); do
  if [[ -s "$data_dir/kubeconfig.yaml" ]]; then
    cp "$data_dir/kubeconfig.yaml" "$kubeconfig"
    kubectl --kubeconfig "$kubeconfig" config set-cluster default --server="https://127.0.0.1:${host_port}" >/dev/null
    if kubectl --kubeconfig "$kubeconfig" --request-timeout=2s get --raw=/readyz >/dev/null 2>&1; then
      break
    fi
  fi
  sleep 1
done
[[ -s "$kubeconfig" ]] || { echo "K8E did not write kubeconfig" >&2; exit 1; }
kubectl --kubeconfig "$kubeconfig" --request-timeout=10s get --raw=/readyz >/dev/null

MCP_TEST_KUBECONFIG="$kubeconfig" \
MCP_TEST_RESTART_CONTAINER="$container_name" \
go test -race ./pkg/sandboxmcp -run '^TestKubernetesRestartRecovery$' -v -count=1

echo "OrbStack K8E cluster-init recovery test passed: $container_name"
