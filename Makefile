COMMIT=$(shell git rev-parse HEAD)
BRANCH=$(shell git rev-parse --abbrev-ref HEAD)
VERSION_K8S=$(grep k8s.io/kubernetes go.mod | head -n1 | awk '{print $4}' | sed -e 's/[-+].*//')
VERSION="${VERSION_K8S}+k8e-${COMMIT:0:8}"
SOURCE_DIRS = cmd pkg lib main.go
LDFLAGS := "-s -w -X main.VERSION=${VERSION} -X main.COMMIT=${COMMIT} -X main.BRANCH=${BRANCH}"

.PHONY: all

.PHONY: deps
deps:
	@go mod vendor
	@go mod tidy

.PHONY: build
build: generate
	@bash ./hack/build

.PHONY: generate
generate:
	@bash ./hack/generate

.PHONY: clean
clean:
	@bash ./hack/clean
	
.PHONY: test
test:
	CGO_ENABLED=0 go test $(shell go list ./... | grep -v /vendor/|xargs echo) -cover

