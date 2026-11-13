.PHONY: build test integration lint

GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo "$$(go env GOPATH)/bin/golangci-lint")

build:
	CGO_ENABLED=0 go build -o dist/agent-sandbox ./cmd/agent-sandbox

test:
	go test ./...

# Needs rootless podman; the first run builds the base image (network access).
integration:
	go test -tags integration -count=1 -timeout 30m ./test/...

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	go vet -tags integration ./...
	$(GOLANGCI_LINT) run ./...
