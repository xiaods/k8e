#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kubeconfig=""
public_cert=""
public_key=""
gateway_ca=""
gateway_cert=""
gateway_key=""
hostname=""
apply=false

usage() {
  cat <<'EOF'
Usage: bash hack/init-sandbox-mcp.sh --kubeconfig FILE \
  --gateway-ca FILE --gateway-cert FILE --gateway-key FILE \
  [--public-cert FILE --public-key FILE] \
  [--hostname MCP_PUBLIC_HOST] [--apply]

Uses existing K8E keys from sandbox-matrix/sandbox-apikeys. No OAuth server is
needed. An existing sandbox-matrix/sandbox-e2b certificate is reused unless a
new public certificate pair is supplied explicitly. The existing k8e API
Gateway exposes https://HOST/mcp. Without --apply, validates local inputs only.
The deployment uses k8e-mcp:local; import that image into K8E containerd first.

Set MCP_PROBE_API_KEY (environment, never argv) to additionally prove the
authenticated MCP path by calling server/discover and requiring HTTP 200.
EOF
}

rollout_failure_diagnostics() {
  echo "k8e-mcp did not become ready; collecting diagnostics:" >&2
  kubectl "${kubectl_args[@]}" -n k8e-mcp get pods -o wide >&2 || true
  kubectl "${kubectl_args[@]}" -n k8e-mcp describe deployment/k8e-mcp >&2 || true
  kubectl "${kubectl_args[@]}" -n k8e-mcp logs deployment/k8e-mcp --all-containers --tail=40 >&2 || true
  echo "check that --gateway-ca/--gateway-cert/--gateway-key are a valid, unexpired mTLS client pair for the sandbox gateway" >&2
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
    --hostname) hostname="$2" ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift 2
done

for file in "$kubeconfig" "$gateway_ca" "$gateway_cert" "$gateway_key"; do
  [[ -n "$file" && -r "$file" && -s "$file" ]] || { echo "required nonempty readable file: $file" >&2; exit 2; }
done
if [[ -n "$public_cert" || -n "$public_key" ]]; then
  [[ -n "$public_cert" && -n "$public_key" ]] || { echo "--public-cert and --public-key must be supplied together" >&2; exit 2; }
  for file in "$public_cert" "$public_key"; do
    [[ -r "$file" && -s "$file" ]] || { echo "required nonempty readable file: $file" >&2; exit 2; }
  done
fi
command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 2; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 2; }
if [[ -n "$hostname" ]]; then
  valid_host=false
  if [[ ${#hostname} -le 253 && "$hostname" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$ ]]; then
    valid_host=true
  fi
  if [[ "$hostname" == *.* && "$hostname" =~ ^[0-9.]+$ ]]; then
    valid_host=true
    IFS=. read -r -a octets <<<"$hostname"
    [[ ${#octets[@]} -eq 4 ]] || valid_host=false
    for octet in "${octets[@]}"; do
      [[ "$octet" =~ ^[0-9]{1,3}$ && $((10#$octet)) -le 255 ]] || valid_host=false
    done
  fi
  [[ "$valid_host" == true ]] || { echo "--hostname must be a DNS name or IPv4 address without scheme, port, path, or whitespace" >&2; exit 2; }
fi

if [[ "$apply" != true ]]; then
  echo "API Key mode; namespace=k8e-mcp; credentials=sandbox-matrix/sandbox-apikeys"
  echo "Local inputs checked. Add --apply to initialize Kubernetes."
  exit 0
fi

kubectl_args=(--kubeconfig "$kubeconfig")
kubectl "${kubectl_args[@]}" -n sandbox-matrix get secret sandbox-apikeys -o name >/dev/null
kubectl "${kubectl_args[@]}" -n sandbox-matrix get gateway e2b -o name >/dev/null
if [[ -z "$public_cert" ]]; then
  kubectl "${kubectl_args[@]}" -n sandbox-matrix get secret sandbox-e2b -o name >/dev/null
fi
kubectl "${kubectl_args[@]}" create namespace k8e-mcp --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply -f -
if [[ -n "$public_cert" ]]; then
  kubectl "${kubectl_args[@]}" -n sandbox-matrix create secret tls sandbox-e2b --cert="$public_cert" --key="$public_key" --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply --server-side --field-manager=k8e-mcp-init -f -
fi
kubectl "${kubectl_args[@]}" -n k8e-mcp create secret generic mcp-gateway-tls --from-file=ca.crt="$gateway_ca" --from-file=tls.crt="$gateway_cert" --from-file=tls.key="$gateway_key" --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply --server-side --field-manager=k8e-mcp-init -f -
kubectl "${kubectl_args[@]}" apply -f "$repo_root/manifests/sandbox-mcp/deployment.yaml"
kubectl "${kubectl_args[@]}" -n k8e-mcp rollout restart deployment/k8e-mcp
if ! kubectl "${kubectl_args[@]}" -n k8e-mcp rollout status deployment/k8e-mcp --timeout=120s; then
  rollout_failure_diagnostics
  exit 1
fi
kubectl "${kubectl_args[@]}" -n sandbox-matrix wait --for=condition=Programmed gateway/e2b --timeout=120s
kubectl "${kubectl_args[@]}" -n sandbox-matrix wait --for=jsonpath='{.status.listeners[?(@.name=="https")].conditions[?(@.type=="Programmed")].status}'=True gateway/e2b --timeout=120s
kubectl "${kubectl_args[@]}" -n sandbox-matrix wait --for=jsonpath='{.status.listeners[?(@.name=="https")].conditions[?(@.type=="ResolvedRefs")].status}'=True gateway/e2b --timeout=120s
kubectl "${kubectl_args[@]}" -n sandbox-matrix wait --for=jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}'=True httproute/k8e-mcp --timeout=120s
kubectl "${kubectl_args[@]}" -n sandbox-matrix wait --for=jsonpath='{.status.parents[0].conditions[?(@.type=="ResolvedRefs")].status}'=True httproute/k8e-mcp --timeout=120s

gateway_address="$(kubectl "${kubectl_args[@]}" -n sandbox-matrix get gateway e2b -o jsonpath='{.status.addresses[0].value}')"
endpoint_host="${hostname:-$gateway_address}"
[[ -n "$endpoint_host" ]] || { echo "k8e API Gateway has no advertised address" >&2; exit 1; }
endpoint_authority="$endpoint_host"
if [[ "$endpoint_authority" == *:* ]]; then
  endpoint_authority="[$endpoint_authority]"
fi

endpoint_cert="$public_cert"
if [[ -z "$endpoint_cert" ]]; then
  endpoint_cert="$(mktemp)"
  trap 'rm -f "$endpoint_cert"' EXIT
  kubectl "${kubectl_args[@]}" -n sandbox-matrix get secret sandbox-e2b -o jsonpath='{.data.tls\.crt}' | openssl base64 -d -A >"$endpoint_cert"
  [[ -s "$endpoint_cert" ]] || { echo "sandbox-matrix/sandbox-e2b has no tls.crt" >&2; exit 1; }
fi
curl_args=(--silent --show-error --output /dev/null --write-out '%{http_code}' --cacert "$endpoint_cert" --connect-timeout 10 --max-time 20)
if [[ -n "$hostname" && ( "$gateway_address" == *:* || "$gateway_address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ) ]]; then
  resolve_address="$gateway_address"
  [[ "$resolve_address" == *:* ]] && resolve_address="[$resolve_address]"
  curl_args+=(--resolve "$hostname:443:$resolve_address")
fi
status="$(curl "${curl_args[@]}" "https://$endpoint_authority/mcp")"
[[ "$status" == 405 ]] || { echo "MCP HTTPS probe expected 405, got $status" >&2; exit 1; }
if [[ -n "${MCP_PROBE_API_KEY:-}" ]]; then
  probe_body='{"jsonrpc":"2.0","id":"init-probe","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
  auth_status="$(curl "${curl_args[@]}" \
    --request POST \
    --header "Authorization: Bearer ${MCP_PROBE_API_KEY}" \
    --header 'Content-Type: application/json' \
    --header 'Accept: application/json, text/event-stream' \
    --header 'MCP-Protocol-Version: 2026-07-28' \
    --header 'Mcp-Method: server/discover' \
    --data "$probe_body" \
    "https://$endpoint_authority/mcp")"
  [[ "$auth_status" == 200 ]] || { echo "authenticated MCP probe expected 200, got $auth_status" >&2; exit 1; }
  echo "Authenticated MCP probe passed (server/discover returned 200)."
fi
echo "MCP endpoint: https://$endpoint_authority/mcp"
