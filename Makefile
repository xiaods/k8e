TARGETS := $(shell ls hack | grep -v \\.sh)

$(TARGETS):
	zig build $@

.PHONY: deps
deps:
	go mod tidy

.DEFAULT_GOAL := all

all:
	zig build all

k8e:
	zig build k8e

clean:
	rm -rf bin .zig-cache zig-out

format:
	go fmt ./...
	zig fmt build.zig
