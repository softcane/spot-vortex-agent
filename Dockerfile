# SpotVortex Agent Dockerfile
# Multi-stage build for production-ready Go binary with bundled model artifacts.
# Uses native cross-compilation (xx) to avoid QEMU overhead and hangs.

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# Install xx for cross-compilation helpers
COPY --from=tonistiigi/xx:master / /

WORKDIR /app

# Install build dependencies: git (host), and cross-compilation toolchain (target)
# We need 'file' to check binary type, and gcc/musl-dev for CGO.
ARG TARGETPLATFORM
RUN apk add --no-cache git file clang lld && \
    xx-apk add --no-cache gcc musl-dev

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary (onnxruntime_go requires CGO).
# xx-go automatically sets GOOS, GOARCH, CC, CXX, and CGO_ENABLED=1 for target platform.
RUN xx-go build -ldflags "-s -w -linkmode external -extldflags '-static'" -o /app/spotvortex-agent ./cmd/agent && \
    xx-verify /app/spotvortex-agent

# Production image
FROM alpine:3.21

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates curl tar

# Copy binary from builder
COPY --from=builder /app/spotvortex-agent /usr/local/bin/spotvortex-agent

# Install ONNX Runtime shared library for target architecture.
ARG TARGETARCH
ARG ORT_VERSION=1.20.1
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ORT_ARCH="x64" ;; \
      arm64) ORT_ARCH="aarch64" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    ORT_TGZ="onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}.tgz"; \
    curl -fsSL -o /tmp/onnxruntime.tgz "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/${ORT_TGZ}"; \
    mkdir -p /tmp/onnxruntime; \
    tar -xzf /tmp/onnxruntime.tgz -C /tmp/onnxruntime --strip-components=1; \
    # The library structure inside tar might vary slightly by version/platform, verify path if needed.
    # Typically lib/libonnxruntime.so...
    # For v1.20.1, it is directly in lib/ usually.
    # Let's ensure directory exists before copy.
    mkdir -p /usr/local/lib/onnxruntime; \
    find /tmp/onnxruntime -name "libonnxruntime.so.${ORT_VERSION}" -exec cp {} /usr/local/lib/onnxruntime/ \; ; \
    rm -rf /tmp/onnxruntime /tmp/onnxruntime.tgz

ENV SPOTVORTEX_ONNXRUNTIME_PATH=/usr/local/lib/onnxruntime

# Bundle minimal runtime artifacts.
COPY config/default.yaml ./config/default.yaml
COPY models/ ./models/

# Manifest verification is a hard startup gate in agent mode.

ENTRYPOINT ["spotvortex-agent"]
CMD ["run"]
