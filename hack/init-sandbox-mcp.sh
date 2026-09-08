#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kubeconfig=""
public_cert=""
public_key=""
gateway_ca=""
gateway_cert=""
gateway_key=""
apply=false

usage() {
  cat <<'EOF'
Usage: bash hack/init-sandbox-mcp.sh --kubeconfig FILE \
  --public-cert FILE --public-key FILE \
  --gateway-ca FILE --gateway-cert FILE --gateway-key FILE [--apply]

Uses existing K8E keys from sandbox-matrix/sandbox-apikeys. No OAuth server is
needed. Without --apply, validates local inputs only. The deployment uses
k8e-mcp:local; import that image into K8E containerd before deployment.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) apply=true; shift; continue ;;
    -h|--help) usage; exit 0 ;;
  esac
  [[ $# -ge 2 && "$2" != --* ]] || { echo "missing option value: $1" >&2; exit 2; }
  case "$1" in
    --kubeconfig) kubeconfig="$2" ;;
    --public-cert) public_cert="$2" ;;
    --public-key) public_key="$2" ;;
    --gateway-ca) gateway_ca="$2" ;;
    --gateway-cert) gateway_cert="$2" ;;
    --gateway-key) gateway_key="$2" ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift 2
done

for file in "$kubeconfig" "$public_cert" "$public_key" "$gateway_ca" "$gateway_cert" "$gateway_key"; do
  [[ -n "$file" && -r "$file" && -s "$file" ]] || { echo "required nonempty readable file: $file" >&2; exit 2; }
done
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 2; }

if [[ "$apply" != true ]]; then
  echo "API Key mode; namespace=k8e-mcp; credentials=sandbox-matrix/sandbox-apikeys"
  echo "Local inputs checked. Add --apply to initialize Kubernetes."
  exit 0
fi

kubectl_args=(--kubeconfig "$kubeconfig")
kubectl "${kubectl_args[@]}" -n sandbox-matrix get secret sandbox-apikeys -o name >/dev/null
kubectl "${kubectl_args[@]}" create namespace k8e-mcp --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply -f -
kubectl "${kubectl_args[@]}" -n k8e-mcp create secret tls mcp-public-tls --cert="$public_cert" --key="$public_key" --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply --server-side --field-manager=k8e-mcp-init -f -
kubectl "${kubectl_args[@]}" -n k8e-mcp create secret generic mcp-gateway-tls --from-file=ca.crt="$gateway_ca" --from-file=tls.crt="$gateway_cert" --from-file=tls.key="$gateway_key" --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply --server-side --field-manager=k8e-mcp-init -f -
kubectl "${kubectl_args[@]}" apply -f "$repo_root/manifests/sandbox-mcp/deployment.yaml"
kubectl "${kubectl_args[@]}" -n k8e-mcp rollout restart deployment/k8e-mcp
kubectl "${kubectl_args[@]}" -n k8e-mcp rollout status deployment/k8e-mcp --timeout=120s
