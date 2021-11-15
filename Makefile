TARGETS := $(shell ls hack | grep -v \\.sh | grep -v package-airgap| grep -v clean)

.dapper:
	@echo Downloading dapper
	@curl -s https://api.github.com/repos/rancher/dapper/releases/latest \
		| grep browser_download_url \
		| grep dapper \
		| cut -d '"' -f 4 \
		| wget -qi -
	@mv dapper .dapper
	@@chmod +x .dapper
	@./.dapper -v
	
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
	# go generate

build/data:
	mkdir -p $@

package-airgap:
	./hack/package-airgap

clean:
	./hack/clean
