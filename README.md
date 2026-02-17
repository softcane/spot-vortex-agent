# SpotVortex Agent

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/softcane/spot-vortex-agent)](https://goreportcard.com/report/github.com/softcane/spot-vortex-agent)

**SpotVortex Agent** is an intelligent, privacy-first Kubernetes operator that optimizes your cluster configurations using Spot instances. By leveraging advanced machine learning models (Temporal Fusion Transformer & Reinforcement Learning), SpotVortex predicts spot instance availability and price fluctuations to steer workloads towards the most cost-effective and reliable nodes.

## 🚀 Key Features

- **🤖 ML-Driven Optimization**: Utilizes embedded ONNX models (TFT & RL) to make real-time, data-driven decisions on node selection.
- **🔒 Privacy-First Architecture**: Designed with strict data sovereignty in mind. **Zero** sensitive customer data (pod names, secrets, env vars) leaves your VPC. Only anonymized pricing and signed billing manifests are exported (opt-in).
- **🛡️ Safety Guardrails**: Includes a "Guardian" component that enforces Pod Disruption Budgets (PDBs) and prevents unsafe evictions or scaling actions.
- **⚡ High Performance**: Written in Go for low resource footprint and high reliability.
- **🤝 Karpenter Integration**: Works seamlessly with Karpenter to provision the right compute at the right time.

## 🏗️ Architecture

SpotVortex operates as a control plane within your cluster, consisting of several key components:

- **Agent (Controller)**: The main control loop that orchestrates actions.
- **Inference Engine**: Runs ONNX models locally to predict spot market behavior.
- **Guardian**: Enforces safety policies and PDBs to ensure cluster stability.
- **Collector**: Gathers local cluster metrics without exposing PDB.

For a deeper technical dive into the runtime architecture and model integration, please see [AGENT_RUNTIME_TECHNICAL.md](docs_internal/AGENT_RUNTIME_TECHNICAL.md).

## 📦 Installation

### Prerequisites

- Kubernetes cluster (EKS supported)
- Helm 3.0+

### One-line Install (Helm OCI)

```bash
helm upgrade --install spotvortex oci://ghcr.io/softcane/charts/spotvortex \
  --namespace spotvortex --create-namespace \
  --set apiKey=<API_KEY>
```

### Install via Script

```bash
curl -fsSL https://raw.githubusercontent.com/softcane/spot-vortex-agent/main/hack/install.sh | SPOTVORTEX_API_KEY=<API_KEY> bash
```

## 🔌 Model Contract

The agent consumes exported model artifacts from the upstream ML pipeline. These models are essential for the agent's operation:

- `models/tft.onnx`
- `models/tft.onnx.data`
- `models/rl_policy.onnx`
- `models/rl_policy.onnx.data`
- `models/MODEL_MANIFEST.json`

Startup enforces the presence of these files and verifies their checksums against the manifest. The model scope (cloud provider + supported instance families) is also enforced from `MODEL_MANIFEST.json`.

## 🛠️ Development & Local Validation

If you want to contribute or run the agent locally for development:

**Build and Test:**
```bash
go list ./... | grep -v '/tests/e2e' | xargs go test -count=1
helm lint charts/spotvortex
helm template spotvortex charts/spotvortex --set apiKey=dummy >/tmp/spotvortex_chart.yaml
docker build -t spotvortex-agent:local .
```

**Run End-to-End Tests:**
```bash
go test -v ./tests/e2e -run TestFullInferencePipeline -count=1
```

## 🤝 Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on how to submit issues and pull requests.

## 📄 License

SpotVortex Agent is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for the full license text.
