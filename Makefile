COMMIT=$(shell git rev-parse HEAD)
BRANCH=$(shell git rev-parse --abbrev-ref HEAD)
VERSION_K8S=$(grep k8s.io/kubernetes go.mod | head -n1 | awk '{print $4}' | sed -e 's/[-+].*//')
VERSION="${VERSION_K8S}+k8e-${COMMIT:0:8}"
SOURCE_DIRS = cmd pkg lib main.go
LDFLAGS := "-s -w -X main.VERSION=${VERSION} -X main.COMMIT=${COMMIT} -X main.BRANCH=${BRANCH}"

all: clean deps build
.PHONY: all

.PHONY: deps
deps:
	@go mod tidy
	@go mod vendor

.PHONY: build
build: build/data
	@bash ./hack/build

generate: build/data
	@go generate

build/data:
	@mkdir -p $@

.PHONY: package
package:
	@bash ./hack/package

.PHONY: clean
clean:
	@bash ./hack/clean

.PHONY: test
test:
	CGO_ENABLED=0 go test $(shell go list ./... | grep -v /vendor/|xargs echo) -cover
