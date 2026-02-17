# SpotVortex Agent Dockerfile
# Multi-stage build for production-ready Go binary with bundled model artifacts.

FROM golang:1.25-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates build-base

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary (onnxruntime_go requires CGO).
RUN CGO_ENABLED=1 GOOS=linux go build -o spotvortex-agent ./cmd/agent

# Production image
FROM alpine:3.21

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates curl tar

# Copy binary from builder
COPY --from=builder /app/spotvortex-agent /usr/local/bin/spotvortex-agent

# Install ONNX Runtime shared library for target architecture.
ARG TARGETARCH
ARG ORT_VERSION=1.23.2
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
    cp "/tmp/onnxruntime/lib/libonnxruntime.so.${ORT_VERSION}" /usr/local/lib/onnxruntime; \
    rm -rf /tmp/onnxruntime /tmp/onnxruntime.tgz

ENV SPOTVORTEX_ONNXRUNTIME_PATH=/usr/local/lib/onnxruntime

# Bundle minimal runtime artifacts.
COPY config/default.yaml ./config/default.yaml
COPY models/ ./models/

# Manifest verification is a hard startup gate in agent mode.

ENTRYPOINT ["spotvortex-agent"]
CMD ["run"]
