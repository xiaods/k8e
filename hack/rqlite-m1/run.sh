#!/usr/bin/env bash
# M1 evidence harness for the rqlite etcd-compat layer (Issue #616, KIP-29).
#
# Runs the M1 suite against a real rqlited plus, where it matters, against the
# embedded etcd so the observable behaviour can be compared one-to-one:
#
#   TestM1DifferentialContracts        - results, errors, revisions, event order
#   TestM1DocumentedDeviations         - the explicit Unimplemented replies
#   TestM1ThreeNode*                   - 3 members reuse the same semantics
#   TestM1LayerRestartContinuity       - MVCC history/leases survive a restart
#   TestM1RangeStreamDifferential      - streaming range vs embedded etcd
#   TestM1RequestSizeLimitsMatchEtcd   - size limits vs an identical embedded etcd
#   TestM1TimeoutLimits                - deadline/cancel paths
#   TestM1TLSAndMTLSServeTheSameContract - TLS/mTLS surface and rejections
#
#   hack/rqlite-m1/run.sh                        # full M1 evidence suite
#   hack/rqlite-m1/run.sh -run TestM1ThreeNode   # extra args go to `go test`
#   RQLITE_M1_RUN=TestM1Wire hack/rqlite-m1/run.sh
#
# The pinned rqlite release (v10.3.5) and its SHA-256 check are owned by
# hack/rqlite-m0/run.sh, which this script reuses: M0 and M1 must run against
# the same verified binary. Like M0, the suite skips itself when RQLITE_BIN is
# unset, so `go test ./...` stays green without this script and without network.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${root}"

exec hack/rqlite-m0/run.sh -run "${RQLITE_M1_RUN:-TestM1}" "$@"
