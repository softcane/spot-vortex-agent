# Contributing to SpotVortex Agent

## Scope

This repository contains the open-source Go agent/runtime only.
ML training/data pipelines are out of scope.

## Prerequisites

- Go (version from `go.mod`)
- Docker
- Helm
- Kind + kubectl (for optional e2e)

## Local checks

```bash
go list ./... | grep -v '/tests/e2e' | xargs go test -count=1
helm template spotvortex charts/spotvortex --set apiKey=dummy >/tmp/spotvortex_chart.yaml
docker build -t spotvortex-agent:local .
```

Optional e2e:

```bash
go test -v ./tests/e2e -run TestFullInferencePipeline -count=1
```

## Pull requests

- Keep changes focused and minimal.
- Add/adjust tests for behavioral changes.
- Do not commit local model bundles in `models/`.
