GO_VERSION=1.16.3
GOLANGCI_LINT_VERSION=1.39.0
GOFUMPT_VERSION=0.1.1

.PHONY : deps fmt lint build test all

all: deps fmt build test

deps:
	go mod tidy
	@test -f ./bin/golangci-lint || curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | BINARY=golangci-lint bash -s -- v$(GOLANGCI_LINT_VERSION)
	@test -f ./bin/gofumpt 	    || curl -sLo ./bin/gofumpt https://github.com/mvdan/gofumpt/releases/download/v$(GOFUMPT_VERSION)/gofumpt_v$(GOFUMPT_VERSION)_darwin_amd64
	@chmod +x ./bin/*

lint:
	./bin/golangci-lint run ./... -c .golangci.yaml

fmt:
	@# - gofumpt is not included in the .golangci.yaml because it conflicts with imports https://github.com/golangci/golangci-lint/issues/1490#issuecomment-778782810
	@# - goimports is not turned on since it is used mostly by gofumpt internally
	./bin/gofumpt -l -w -extra .
	./bin/golangci-lint run ./... -c .golangci.yaml --fix

test:
	go test -race ./...

build:
	go build -ldflags="-s -w" -o kubeclientlib ./client && rm kubeclientlib

