TARGETS := $(shell ls hack | grep -v \\.sh | grep -v package-airgap| grep -v clean)
DAPPER := dapper-Linux-x86_64
ifeq ($(GOARCH), arm64)
	DAPPER := dapper-Linux-arm64
endif

.dapper:
	@echo Downloading dapper
	@curl -s https://api.github.com/repos/rancher/dapper/releases/latest \
		| grep browser_download_url \
		| grep $(DAPPER) \
		| cut -d '"' -f 4 \
		| wget -qi - -O dapper
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
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go generate

build/data:
	mkdir -p $@

package-airgap:
	./hack/package-airgap

clean:
	./hack/clean
