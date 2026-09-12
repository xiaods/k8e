#!/usr/bin/env bash
# Legacy entry point for the local "cluster-init recovery" test.
#
# The real harness now lives in hack/e2e/. This wrapper only maps the historical
# K8E_MCP_* variables onto the harness environment so existing muscle memory and
# documentation keep working.
#
#   K8E_BINARY=/path/to/linux-k8e hack/test-sandbox-mcp-orbstack.sh
#
# See `hack/e2e/up.sh --help` for the underlying knobs.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"

export E2E_PROFILE="${E2E_PROFILE:-l1}"
export E2E_CONTAINER="${E2E_CONTAINER:-${K8E_MCP_CONTAINER:-k8e-e2e-l1}}"
export E2E_API_PORT="${E2E_API_PORT:-${K8E_MCP_API_PORT:-16444}}"
export E2E_STATE_DIR="${E2E_STATE_DIR:-/tmp/k8e-e2e/${E2E_PROFILE}}"
export E2E_BINARY="${E2E_BINARY:-${K8E_BINARY:-${repo}/bin/k8e}}"
# The recovery test is the one place where the race detector earns its cost.
export E2E_GO_TEST_ARGS="${E2E_GO_TEST_ARGS:--race}"
# Historically the cluster was left behind for post-mortem debugging.
export E2E_KEEP="${E2E_KEEP:-${K8E_MCP_KEEP:-1}}"

if [ -n "${K8E_MCP_DATA_DIR:-}" ]; then
    export E2E_DATA_DIR="${K8E_MCP_DATA_DIR}"
fi

if [ -n "${K8E_MCP_IMAGE:-}" ]; then
    export E2E_IMAGE="${K8E_MCP_IMAGE}"
    export E2E_SKIP_IMAGE_BUILD=1
fi

exec bash "${repo}/hack/e2e/run.sh" "${E2E_PROFILE}" "$@"
