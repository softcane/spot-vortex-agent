# SpotVortex Agent Makefile
# Build, test, and development automation

BINARY_NAME := agent
BUILD_DIR := bin
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')
VERSION ?= dev

# Go build settings
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
LDFLAGS := -ldflags "-s -w -X github.com/softcane/spot-vortex-agent/cmd/agent/cmd.buildVersion=$(VERSION)"

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

## test-unit: Run unit tests (excluding E2E)
test-unit:
	@echo "==> Running unit tests..."
	go list ./... | grep -v '/tests/e2e' | xargs go test -count=1

## test-fault-injection: Run fault injection tests
test-fault-injection:
	@echo "==> Running fault injection tests..."
	go test -count=1 ./internal/capacity -run 'TestKarpenterManager_PrepareSwap_(MissingNodePools|PatchDenied|PartialPatchSuccessStillReady)'
	go test -count=1 ./internal/controller -run 'TestDrain_AbortsWhenPDBBlocks'

## test-e2e-eksa: Run EKS Anywhere E2E tests
test-e2e-eksa:
	@echo "==> Running EKS Anywhere E2E tests..."
	go test -v ./tests/e2e -run 'TestEKSAnywhereCA_' -count=1

## test-e2e-karpenter: Run Karpenter local E2E tests
test-e2e-karpenter:
	@echo "==> Running Karpenter local E2E tests..."
	go test -v ./tests/e2e -run 'TestKarpenterLocal_' -count=1

## docker-build: Build Docker image
docker-build:
	@echo "==> Building Docker image..."
	docker build --build-arg VERSION=$(VERSION) -t spotvortex-agent:local .

## helm-lint: Lint and template Helm chart
helm-lint:
	@echo "==> Linting Helm chart..."
	helm lint charts/spotvortex
	helm template spotvortex charts/spotvortex >/tmp/spotvortex_chart.yaml

## helm-package: Package Helm chart
helm-package:
	@echo "==> Packaging Helm chart..."
	mkdir -p dist
	helm dependency update charts/spotvortex || true
	helm package charts/spotvortex --destination dist

## verify-release-kind: Build local image and verify helm/script installs on kind
verify-release-kind:
	@echo "==> Verifying release install paths on kind..."
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "Set VERSION=vX.Y.Z (or a release-candidate tag) before running verify-release-kind"; \
		exit 1; \
	fi
	docker build --build-arg VERSION=$(VERSION) -t spotvortex-agent:$(VERSION) .
	CHART_REF=charts/spotvortex \
	CHART_VERSION= \
	FORCE_IMAGE_OVERRIDE=1 \
	EXPECTED_IMAGE_REPOSITORY=spotvortex-agent \
	EXPECTED_IMAGE_TAG=$(VERSION) \
	KIND_LOAD_IMAGE=spotvortex-agent:$(VERSION) \
	bash hack/verify-release-kind-install.sh

## help: Show this help message
help:
	@echo "SpotVortex Agent - Makefile Commands"
	@echo ""
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
