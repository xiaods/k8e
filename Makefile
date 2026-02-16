.DEFAULT_GOAL := all

.PHONY: all k8e clean deps format generate package package-cli package-airgap

all:
	zig build all

k8e:
	zig build k8e

clean:
	rm -rf bin .zig-cache zig-out

deps:
	go mod tidy

format:
	go fmt ./...
	zig fmt build.zig

generate:
	hack/generate

package:
	hack/package

package-cli:
	hack/package-cli

package-airgap:
	hack/package-airgap.sh
