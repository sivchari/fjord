.PHONY: build rask-init test test-integration lint lint-fix fmt fmt-diff generate clean tools mod

BINARY_NAME=fjord
VERSION?=$(shell grep 'const Version' version.go | cut -d'"' -f2)
BUILD_DIR=bin
GOLANGCI_LINT=go tool -modfile tools/go.mod golangci-lint
GOTOOLCHAIN=go1.26.3
export GOTOOLCHAIN

RASK_INIT_EMBED=internal/rask/embedded/rask-init

# rask-init cross-compiles the VM's PID 1 over the checked-in placeholder.
# Only macOS clusters boot it; rask cannot ship it inside its own module, so
# fjord embeds it and hands it over via raskcluster.WithRaskInit.
#
# It builds through raskinit/go.mod, not fjord's own: rask-init imports
# packages fjord never does, and `go mod tidy` on the main module prunes
# them. Writing to a temp file first keeps the checked-in placeholder intact
# when the build fails -- go:embed has nothing to embed otherwise.
rask-init:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -modfile raskinit/go.mod -ldflags="-s -w" -o $(RASK_INIT_EMBED).tmp github.com/sivchari/rask/cmd/rask-init
	mv $(RASK_INIT_EMBED).tmp $(RASK_INIT_EMBED)

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
