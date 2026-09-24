.DEFAULT_GOAL := all

# End-to-end profile run by `make e2e` (l1 | l2 | l3). Requires a linux/amd64
# bin/k8e and a running docker daemon.
E2E_PROFILE ?= l1

# Extra args for `make test-rqlite-m0`, e.g. RQLITE_M0_TEST_ARGS='-run TestTxn'.
RQLITE_M0_TEST_ARGS ?=

.PHONY: all k8e tandem clean deps format generate package package-cli package-airgap test test-rqlite-m0 e2e

all:
	zig build all

tandem:
	cd tandem && zig build
	@mkdir -p bin
	@arch=$$(uname -m); \
	case "$$arch" in \
		x86_64) tarch="x86_64-linux-musl" ;; \
		arm64|aarch64) tarch="aarch64-linux-musl" ;; \
		*) tarch="$$arch-linux-musl" ;; \
	esac; \
	if [ -f "tandem/bin/tandem-$$tarch" ]; then \
		cp -f "tandem/bin/tandem-$$tarch" bin/tandem; \
	fi

k8e: tandem
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

e2e:
	hack/e2e/run.sh $(E2E_PROFILE)
