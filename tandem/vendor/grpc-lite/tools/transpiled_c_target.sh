#!/usr/bin/env bash
# Shared target selection for generated C bundles. Source after setting project_root.
transpiled_c_target=${GRPC_LITE_TRANSPILED_C_TARGET:-}
if [[ -z "$transpiled_c_target" ]]; then
    case "$(uname -m)" in
        x86_64|amd64) transpiled_c_target=x86_64-linux-gnu ;;
        aarch64|arm64) transpiled_c_target=aarch64-linux-gnu ;;
        *) printf 'Unsupported transpiled C host architecture: %s\n' "$(uname -m)" >&2; exit 2 ;;
    esac
fi
case "$transpiled_c_target" in
    x86_64-linux-gnu) transpiled_c_arch=x86_64 ;;
    aarch64-linux-gnu) transpiled_c_arch=arm64 ;;
    *) printf 'Unsupported transpiled C target: %s\n' "$transpiled_c_target" >&2; exit 2 ;;
esac
export GRPC_LITE_TRANSPILED_C_TARGET="$transpiled_c_target"
