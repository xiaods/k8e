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

RQLITE_VERSION="v10.3.5"
RQLITE_BASE_URL="${RQLITE_BASE_URL:-https://github.com/rqlite/rqlite/releases/download}"
GO="${GO:-go}"

# SHA-256 digests published on the release page of https://github.com/rqlite/rqlite
# (also returned by the GitHub releases API as asset digests).
case "$(uname -s)/$(uname -m)" in
Linux/x86_64 | Linux/amd64)
    asset="rqlite-${RQLITE_VERSION}-linux-amd64.tar.gz"
    digest="edaef7580cd60f7f9d558cde2fb95011f6e356ff64da7167fb659ab4b6882014"
    ;;
Linux/aarch64 | Linux/arm64)
    asset="rqlite-${RQLITE_VERSION}-linux-arm64.tar.gz"
    digest="23d5d09d34a9e2db54b2c21469aacb3b6563166e3d0d3901f4f4fb44990db99b"
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
