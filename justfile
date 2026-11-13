# The binary is copied into the container, so it must be static.
export CGO_ENABLED := "0"

# List recipes.
_default:
    @just --list

install:
    go install ./cmd/agent-sandbox

build:
    go build -o dist/agent-sandbox ./cmd/agent-sandbox

test:
    go test ./...

# Needs rootless podman; the first run builds the base image (network access).
integration:
    go test -tags integration -count=1 -timeout 30m ./test/...

# Override the linter binary with GOLANGCI_LINT=/path/to/golangci-lint.
lint:
    gofmt -l . | (! grep .)
    go vet ./...
    go vet -tags integration ./...
    golangci-lint run --verbose ./...
