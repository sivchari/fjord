.PHONY: build test test-integration lint lint-fix fmt fmt-diff generate clean tools mod

BINARY_NAME=fjord
VERSION?=$(shell grep 'const Version' version.go | cut -d'"' -f2)
BUILD_DIR=bin
GOLANGCI_LINT=go tool -modfile tools/go.mod golangci-lint
GOTOOLCHAIN=go1.25.10
export GOTOOLCHAIN

# Build
build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/fjord

# Test
test:
	go test -race -shuffle=on ./...

test-cover:
	go test -race -coverprofile=coverage.out -coverpkg=./... ./...
	go tool cover -html=coverage.out -o coverage.html

test-integration:
	go test -C test -v -tags=integration -timeout 30m ./integration/...

# Lint
lint:
	$(GOLANGCI_LINT) run ./...

lint-fix:
	$(GOLANGCI_LINT) run --fix ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

fmt-diff:
	$(GOLANGCI_LINT) fmt ./... --diff

# Regenerate internal/eksd/generated_table.go from live EKS-D release manifests.
generate:
	go run ./internal/cmd/eksd-gen

# Clean
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

# Tools
tools:
	cd tools && go mod tidy

# Go mod
mod:
	go mod tidy
