TARGETS := $(shell ls hack | grep -v \\.sh)

.dapper:
	@echo Downloading dapper
	@curl -sL https://releases.rancher.com/dapper/v0.5.7/dapper-$$(uname -s)-$$(uname -m) > .dapper.tmp
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

build/data:
	mkdir -p $@

package-airgap:
	./hack/package-airgap.sh

