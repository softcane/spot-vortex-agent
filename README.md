# SpotVortex Agent

Kubernetes operator runtime for SpotVortex. This repository contains only the Go agent/control-plane runtime.

## What is included

- Go agent (`cmd/`, `internal/`)
- Helm chart (`charts/spotvortex`)
- Kind/KWOK e2e tests (`tests/e2e`)

## What is not included

- TFT/RL/PySR training pipelines
- data preparation and training datasets

## Model contract

The agent consumes exported model artifacts from the ML pipeline:

- `models/tft.onnx`
- `models/tft.onnx.data`
- `models/rl_policy.onnx`
- `models/rl_policy.onnx.data`
- `models/MODEL_MANIFEST.json`

Startup enforces manifest presence and checksum verification.

## One-line install (Helm OCI)

```bash
helm upgrade --install spotvortex oci://ghcr.io/<org>/charts/spotvortex \
  --namespace spotvortex --create-namespace \
  --set apiKey=<API_KEY>
```

Install script option:

```bash
curl -fsSL https://raw.githubusercontent.com/<org>/spot-vortex-agent/main/hack/install.sh | SPOTVORTEX_API_KEY=<API_KEY> bash
```

## Local validation

```bash
go list ./... | grep -v '/tests/e2e' | xargs go test -count=1
helm lint charts/spotvortex
helm template spotvortex charts/spotvortex --set apiKey=dummy >/tmp/spotvortex_chart.yaml
docker build -t spotvortex-agent:local .
```

Optional e2e:

```bash
go test -v ./tests/e2e -run TestFullInferencePipeline -count=1
```
