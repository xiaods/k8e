.DEFAULT_GOAL := all

.PHONY: all k8e clean deps format generate package package-cli package-airgap test

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
