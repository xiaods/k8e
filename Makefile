TARGETS := $(shell ls hack | grep -v \\.sh)
GO_FILES ?= $$(find . -name '*.go' | grep -v generated)

.dapper:
	@echo Downloading dapper
	@curl -sL https://releases.rancher.com/dapper/v0.6.0/dapper-$$(uname -s)-$$(uname -m) > .dapper.tmp
	@@chmod +x .dapper.tmp
	@./.dapper.tmp -v
	@mv .dapper.tmp .dapper

$(TARGETS): .dapper
	./.dapper $@

.PHONY: deps
deps:
	go mod tidy


.DEFAULT_GOAL := ci

.PHONY: $(TARGETS)

build/data:
	mkdir -p $@

package-airgap:
	./hack/package-airgap.sh

format:
	gofmt -s -l -w $(GO_FILES)
	goimports -w $(GO_FILES)
