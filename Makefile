SHELL := /bin/bash

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X github.com/NeuralTrust/ntctl/internal/version.Version=$(VERSION) \
  -X github.com/NeuralTrust/ntctl/internal/version.GitCommit=$(GIT_COMMIT) \
  -X github.com/NeuralTrust/ntctl/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ntctl ./cmd/ntctl

.PHONY: test
test:
	go test -race -coverprofile=coverage.txt -covermode=atomic ./...

# Installs on the cluster in NTCTL_E2E_KUBECONFIG, e.g. a local kind cluster:
#   kind get kubeconfig --name kind > /tmp/kind.kubeconfig
#   NTCTL_E2E_KUBECONFIG=/tmp/kind.kubeconfig make test-e2e
.PHONY: test-e2e
test-e2e:
	go test -tags e2e -run E2E -count=1 -timeout 15m -v ./cmd/ntctl/

.PHONY: lint
lint:
	golangci-lint run --timeout=5m ./...

.PHONY: tidy
tidy:
	go mod tidy

# Builds every release target locally without publishing.
.PHONY: snapshot
snapshot:
	goreleaser release --snapshot --clean

.PHONY: clean
clean:
	rm -rf bin dist coverage.txt
