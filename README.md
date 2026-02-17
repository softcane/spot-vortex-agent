# SpotVortex Agent (Split Workspace)

This directory is a copy-ready workspace for the open-source `spot-vortex-agent` repository.

## Included

- `cmd/` (agent CLI)
- `internal/` (controller, inference, cloud integrations)
- `charts/spotvortex/` (Helm chart)
- `tests/e2e/` (Kind/KWOK e2e tests)
- `config/` runtime defaults (`default.yaml`, `kind.yaml`, `runtime.json`, `workload_distributions.yaml`)
- `dashboards/spotvortex-dryrun.json`

## Not Included

- ML training package (`vortex/`)
- Data prep and training scripts (`scripts/data_prep/`, TFT/RL/PySR runners)
- Datasets, checkpoints, and private experiment artifacts

## Model Contract

Runtime expects pre-exported artifacts:

- `models/tft.onnx`
- `models/tft.onnx.data`
- `models/rl_policy.onnx`
- `models/rl_policy.onnx.data`
- `models/MODEL_MANIFEST.json`

Agent startup enforces manifest validation and checksum verification.

## Quick validation

```bash
# Core packages (works without model bundle)
go list ./... | rg -v '/tests/e2e' | xargs go test -count=1

# Helm render
helm template spotvortex charts/spotvortex --set apiKey=dummy >/tmp/spotvortex_chart.yaml

# E2E requires model artifacts in ./models
go test -v ./tests/e2e -run TestFullInferencePipeline -count=1
```
