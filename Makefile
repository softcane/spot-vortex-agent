# SpotVortex Agent Makefile
# Build, test, and development automation

BINARY_NAME := agent
BUILD_DIR := bin
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')

# Go build settings
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
LDFLAGS := -ldflags "-s -w"

.PHONY: all build test lint clean dev-env help

all: build

## build: Compile the agent binary
build:
	@echo "==> Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/agent

## test: Run all tests with coverage
test:
	@echo "==> Running tests..."
	go test -v -race -cover ./...

## lint: Run golangci-lint
lint:
	@echo "==> Running linter..."
	golangci-lint run ./...

## clean: Remove build artifacts
clean:
	@echo "==> Cleaning..."
	@rm -rf $(BUILD_DIR)
	go clean

## dev-env: Set up development environment
dev-env:
	@echo "==> Setting up dev environment..."
	go mod tidy
	go mod download

## run-dry: Run the agent in dry-run mode (safe for testing)
run-dry: build
	@echo "==> Running agent in dry-run mode..."
	./$(BUILD_DIR)/$(BINARY_NAME) run --dry-run

## help: Show this help message
help:
	@echo "SpotVortex Agent - Makefile Commands"
	@echo ""
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
