TARGETS := $(shell ls hack | grep -v \\.sh)

.dapper:
	@echo Downloading dapper
	@curl -sL https://releases.rancher.com/dapper/v0.5.1/dapper-$$(uname -s)-$$(uname -m) > .dapper.tmp
	@@chmod +x .dapper.tmp
	@./.dapper.tmp -v
	@mv .dapper.tmp .dapper

$(TARGETS): .dapper
	./.dapper $@

.PHONY: deps
deps:
	go mod vendor
	go mod tidy


.DEFAULT_GOAL := ci

.PHONY: $(TARGETS)

.PHONY: generate
generate: build/data
	./hack/download
	go generate

build/data:
	mkdir -p $@
