.DEFAULT_GOAL := all

# End-to-end profile run by `make e2e` (l1 | l2 | l3). Requires a linux/amd64
# bin/k8e and a running docker daemon.
E2E_PROFILE ?= l1

.PHONY: all k8e clean deps format generate package package-cli package-airgap test e2e

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

e2e:
	hack/e2e/run.sh $(E2E_PROFILE)
