# SpotVortex Agent Dockerfile
# Multi-stage build for production-ready Go binary with bundled model artifacts.
# NOTE: ONNX Runtime loading requires CGO with dynamic linking (no static binary).

FROM --platform=$TARGETPLATFORM golang:1.25-bookworm AS builder

WORKDIR /app

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates build-essential && \
    rm -rf /var/lib/apt/lists/*

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary (onnxruntime_go requires CGO and dynamic loading support).
RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w -X github.com/softcane/spot-vortex-agent/cmd/agent/cmd.buildVersion=${VERSION}" \
    -o /app/spotvortex-agent ./cmd/agent

# Production image
FROM --platform=$TARGETPLATFORM debian:bookworm-slim

WORKDIR /app

# Install runtime dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl tar && \
    rm -rf /var/lib/apt/lists/*

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
    mkdir -p /usr/local/lib/onnxruntime; \
    find /tmp/onnxruntime -name "libonnxruntime.so*" -exec cp {} /usr/local/lib/onnxruntime/ \; ; \
    rm -rf /tmp/onnxruntime /tmp/onnxruntime.tgz

ENV SPOTVORTEX_ONNXRUNTIME_PATH=/usr/local/lib/onnxruntime/libonnxruntime.so.${ORT_VERSION}

# Bundle minimal runtime artifacts.
COPY config/default.yaml ./config/default.yaml
COPY models/ ./models/

# Manifest verification is a hard startup gate in agent mode.

ENTRYPOINT ["spotvortex-agent"]
CMD ["run"]
