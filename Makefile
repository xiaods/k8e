.DEFAULT_GOAL := all

# End-to-end profile run by `make e2e` (l1 | l2 | l3). Requires a linux/amd64
# bin/k8e and a running docker daemon.
E2E_PROFILE ?= l1

# Extra args for `make test-rqlite-m0`, e.g. RQLITE_M0_TEST_ARGS='-run TestTxn'.
RQLITE_M0_TEST_ARGS ?=

# Extra args for `make test-rqlite-m1`, e.g. RQLITE_M1_TEST_ARGS='-run TestM1ThreeNode'.
RQLITE_M1_TEST_ARGS ?=

.PHONY: all k8e clean deps format generate package package-cli package-airgap test test-rqlite-m0 test-rqlite-m1 e2e

all:
	zig build all

k8e:
	zig build k8e

clean:
	zig build clean

deps:
	zig build deps

format:
	zig build fmt

generate:
	zig build generate

package:
	zig build package

package-cli:
	zig build package-cli

package-airgap:
	zig build package-airgap

test:
	zig build test

# M0 evidence for KIP-29: downloads and sha256-verifies the pinned rqlited,
# then runs tests/rqlitecompat against it. Skips nothing: RQLITE_BIN is set.
test-rqlite-m0:
	hack/rqlite-m0/run.sh $(RQLITE_M0_TEST_ARGS)

# M1 evidence for KIP-29: the compatibility layer's differential suite against
# the embedded etcd (plus three-node, restart, TLS and wire-limit checks), on
# the same pinned and sha256-verified rqlited as M0.
test-rqlite-m1:
	hack/rqlite-m1/run.sh $(RQLITE_M1_TEST_ARGS)

e2e:
	hack/e2e/run.sh $(E2E_PROFILE)
