#!/usr/bin/env bash
# Run one K8E end-to-end profile: up -> suites -> diagnostics on failure -> down.
#
#   hack/e2e/run.sh <profile> [suite...]
#
# Suites default to the profile name and live in hack/e2e/suites/<name>.sh.
# E2E_KEEP=1 leaves the cluster running for interactive debugging.
set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

if [[ "${1:-}" = "-h" ]] || [[ "${1:-}" = "--help" ]] || [[ -z "${1:-}" ]]; then
    e2e_usage
    exit 0
fi

e2e_set_profile "$1"
shift

suites=("$@")
if [[ ${#suites[@]} -eq 0 ]]; then
    suites=("${E2E_PROFILE}")
fi

status=0
for suite in "${suites[@]}"; do
    script="${E2E_DIR}/suites/${suite}.sh"
    [[ -f "${script}" ]] || e2e_die "unknown suite '${suite}' (${script} not found); suites/l1.sh and suites/l2.sh are implemented so far"
done

# A bring-up failure is reported and diagnosed exactly like a suite failure:
# otherwise up.sh dying (containerd refusing to start, the API never becoming
# ready) would skip dump-logs.sh and leave CI without a single artifact.
if "${E2E_DIR}/up.sh" "${E2E_PROFILE}"; then
    for suite in "${suites[@]}"; do
        script="${E2E_DIR}/suites/${suite}.sh"
        e2e_log "running suite ${suite}"
        if ! bash "${script}"; then
            e2e_bad "suite ${suite} failed"
            status=1
        fi
    done
else
    e2e_bad "cluster bring-up failed"
    status=1
fi

if [[ "${status}" -ne 0 ]]; then
    e2e_log "collecting diagnostics after failure"
    bash "${E2E_DIR}/dump-logs.sh" "${E2E_PROFILE}" || true
fi

if [[ "${E2E_KEEP}" = "1" ]]; then
    e2e_log "E2E_KEEP=1: leaving ${E2E_CONTAINER} running"
else
    bash "${E2E_DIR}/down.sh" "${E2E_PROFILE}" || true
fi

if [[ "${status}" -ne 0 ]]; then
    printf '\n[e2e:%s] E2E FAILED\n' "${E2E_PROFILE}"
else
    printf '\n[e2e:%s] E2E PASSED\n' "${E2E_PROFILE}"
fi
exit "${status}"
