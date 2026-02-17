# Cut-And-Paste Instructions (B1)

## 1) Copy this workspace to a new repository location

```bash
rsync -a --delete split/spot-vortex-agent/ /path/to/spot-vortex-agent/
cd /path/to/spot-vortex-agent
```

## 2) Initialize and push

```bash
git init
git add .
git commit -m "Initial import: spot-vortex-agent"
# add remote + push
```

## 3) Prepare runtime model artifacts

Provide the model bundle in `models/` from your ML export pipeline:

- `tft.onnx`, `tft.onnx.data`
- `rl_policy.onnx`, `rl_policy.onnx.data`
- `MODEL_MANIFEST.json`

## 4) Validate before first release

```bash
go list ./... | rg -v '/tests/e2e' | xargs go test -count=1
helm template spotvortex charts/spotvortex --set apiKey=dummy >/tmp/spotvortex_chart.yaml
go test -v ./tests/e2e -run TestFullInferencePipeline -count=1
```
