#!/usr/bin/env bash
# M0 evidence harness for the rqlite etcd-compat backend (Issue #614, KIP-29).
#
# Fetches the pinned rqlite release, verifies its SHA-256, and runs the
# tests/rqlitecompat suite against that real rqlited binary. Nothing here is
# mocked: the tests start and kill actual rqlite nodes.
#
#   hack/rqlite-m0/run.sh                     # full M0 evidence suite
#   hack/rqlite-m0/run.sh -run TestTxn        # arguments are passed to `go test`
#   RQLITE_M0_CACHE=/tmp/rq hack/rqlite-m0/run.sh
#
# The suite skips itself when RQLITE_BIN is unset, so `go test ./...` stays
# green without this script (and without network access).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${root}"
. ./hack/version.sh

# VERSION_RQLITE is owned by hack/version.sh — every consumer of the pinned
# release (hack/download, hack/e2e/Dockerfile, this harness) reads it from
# there, so a version change is one edit instead of four.
RQLITE_VERSION="${VERSION_RQLITE}"
RQLITE_BASE_URL="${RQLITE_BASE_URL:-https://github.com/rqlite/rqlite/releases/download}"
GO="${GO:-go}"

# SHA-256 digests published on the release page of https://github.com/rqlite/rqlite
# (also returned by the GitHub releases API as asset digests). Update both
# arches together when VERSION_RQLITE changes in hack/version.sh.
case "$(uname -s)/$(uname -m)" in
Linux/x86_64 | Linux/amd64)
    asset="rqlite-${RQLITE_VERSION}-linux-amd64.tar.gz"
    digest="17df480e697e135641fc66aac6c2e7d938c021311e1a8500070e730ccdfa8aa0"
    ;;
Linux/aarch64 | Linux/arm64)
    asset="rqlite-${RQLITE_VERSION}-linux-arm64.tar.gz"
    digest="55d6189a6544361cb3ee357f652534adf14a22af665c5784e22fe83c45c523e1"
    ;;
*)
    echo "[ERROR] no pinned rqlite build for $(uname -s)/$(uname -m); rqlite publishes linux tarballs only" >&2
    exit 1
    ;;
esac

cache="${RQLITE_M0_CACHE:-${root}/bin/rqlite-m0/${RQLITE_VERSION}}"
bin="${cache}/rqlited"

if [[ ! -x "${bin}" ]]; then
    tarball="${cache}/${asset}"
    mkdir -p "${cache}"
    if [[ ! -f "${tarball}" ]]; then
        echo "[rqlite-m0] downloading ${asset}"
        curl -fsSL -o "${tarball}.part" "${RQLITE_BASE_URL}/${RQLITE_VERSION}/${asset}"
        mv "${tarball}.part" "${tarball}"
    fi
    echo "${digest}  ${tarball}" | sha256sum -c -
    tar -xzf "${tarball}" -C "${cache}" --strip-components=1
    rm -f "${tarball}"
fi

echo "[rqlite-m0] $("${bin}" -version)"
RQLITE_BIN="${bin}" exec "${GO}" test -count=1 -v "$@" ./tests/rqlitecompat/
