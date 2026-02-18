# SpotVortex Agent

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/softcane/spot-vortex-agent)](https://goreportcard.com/report/github.com/softcane/spot-vortex-agent)

> **North Star (Project Gate)**
> Prove cost uplift without reliability regression: `eval/net_profit_improvement_pct >= +2.0` (median across seeds) and `eval/outage_delta <= 0` on every seed.

SpotVortex is an open-source Kubernetes operator for one hard problem: getting Spot savings without gambling on reliability.

## Problem

Most teams face the same tradeoff:

- Stay mostly On-Demand and overpay.
- Push Spot aggressively and risk disruption during price spikes or capacity crunches.

Karpenter and Cluster Autoscaler are excellent provisioning primitives, but they are not a full risk-and-economics decision layer for workload migration under changing market conditions.

## Solution

SpotVortex runs in-cluster and continuously decides when to favor Spot vs On-Demand using local telemetry, model inference, and safety guardrails.

- **Local inference**: ONNX models run inside the cluster.
- **Safety gates**: confidence thresholds, PDB-aware draining, bounded drain ratios.
- **Provisioner-aware**: works with Karpenter and ASG-based environments (Cluster Autoscaler / Managed Nodegroup).
- **Deterministic testing**: local E2E harnesses with scripted fake price scenarios for edge cases.

## Mission

Make cost optimization production-safe by default.

Concretely: help platform teams adopt Spot with measurable savings, bounded risk, and auditable behavior.

## Architecture At A Glance

```text
Workloads + Node Metrics + Spot Prices
                 |
                 v
          Collector (in-cluster)
                 |
                 v
   Inference Engine (TFT -> policy action)
                 |
                 v
  Safety Gates (confidence, PDB, drain limits)
                 |
                 v
            Capacity Router
            /             \
           v               v
 Karpenter Manager    ASG Manager (CA/MNG)
 (NodePool weights)   (prepare replacement)
           \               /
            v             v
        Guarded Drainer (shadow/real)
                 |
                 v
      Cluster state + savings telemetry
```

All decisions execute inside the cluster boundary.

## Why SpotVortex vs Plain Karpenter/CA

SpotVortex is **not** a replacement for Karpenter/CA. It is a decision layer on top.

| Capability | Karpenter / CA alone | SpotVortex adds |
| --- | --- | --- |
| Provisioning & scaling | Yes | Uses existing provisioners |
| Market risk scoring | No native ML risk policy | TFT + policy action selection |
| Cost/risk actioning | Indirect | Explicit action space (`HOLD`, `INCREASE_*`, `DECREASE_*`, `EMERGENCY_EXIT`) |
| Model scope enforcement | N/A | Manifest cloud/family scope checks |
| Guarded migration logic | Basic primitives | Confidence gates + PDB-aware drain flow + throttling |
| Deterministic fault simulation | Limited | Local fake price provider + fault-injection suites |

## Model Transparency (Baked In)

This repository does not rely on a remote model link to define behavior. Runtime support is defined by the local model bundle and manifest:

- `models/tft.onnx`
- `models/tft.onnx.data`
- `models/rl_policy.onnx`
- `models/rl_policy.onnx.data`
- `models/MODEL_MANIFEST.json`

Current manifest (`models/MODEL_MANIFEST.json`) states:

- `generated_at`: `2026-02-17T09:54:00Z`
- `cloud`: `aws`
- `supported_instance_families` count: `60`

Supported families (current baked bundle):

`c5, c5a, c5ad, c5d, c5n, c6a, c6g, c6gd, c6gn, c6i, c6id, c6in, c7a, c7g, c7gd, c7gn, c7i, c7i-flex, m5, m5a, m5ad, m5d, m5dn, m5n, m5zn, m6a, m6g, m6gd, m6i, m6id, m6idn, m6in, m7a, m7g, m7gd, m7i, m7i-flex, r5, r5a, r5ad, r5b, r5d, r5dn, r5n, r6a, r6g, r6gd, r6i, r6id, r6idn, r6in, r7a, r7g, r7gd, r7i, r7iz, t2, t3, t3a, t4g`

Model/action contract at runtime:

- TFT model is expected to provide `capacity_score` and `runtime_score`.
- RL model outputs `q_values` over actions: `HOLD`, `DECREASE_10`, `DECREASE_30`, `INCREASE_10`, `INCREASE_30`, `EMERGENCY_EXIT`.
- Runtime policy modes supported by config: `rl` and `deterministic`.
- Startup verifies artifact checksums from manifest.
- Cloud mismatch between config and manifest fails startup.
- Unsupported instance families are forced into the safety fallback action (`EMERGENCY_EXIT`) and counted via `unsupported_instance_family_total`.

Inspect exact current contract locally:

```bash
jq '{generated_at, cloud, supported_instance_families, artifacts}' models/MODEL_MANIFEST.json
```

Reference model card (supplementary):
- [SpotVortex TFT Dual-Head Risk Model (ONNX)](https://huggingface.co/softcane/spot-vortex)

## How It Works

1. Collect node + market telemetry.
2. Run dual-head inference and policy action selection.
3. Apply capacity steering (for example, Karpenter NodePool weight patches).
4. Execute guarded drain/migration flows.
5. Fail safe when confidence or safety conditions are not met.

## Repository Scope

This repo contains the Go agent/runtime and deployment assets.
Model training/data pipelines are out of scope for this repository.

## Quick Start

### Install with Helm

```bash
helm upgrade --install spotvortex oci://ghcr.io/softcane/charts/spotvortex \
  --namespace spotvortex --create-namespace
```

### Install with Script

```bash
curl -fsSL https://raw.githubusercontent.com/softcane/spot-vortex-agent/main/hack/install.sh | bash
```

## Local Development

### Fast local checks

```bash
go list ./... | grep -v '/tests/e2e' | xargs go test -count=1
helm lint charts/spotvortex
helm template spotvortex charts/spotvortex >/tmp/spotvortex_chart.yaml
docker build -t spotvortex-agent:local .
```

### Karpenter local E2E suite

```bash
SPOTVORTEX_E2E_SUITE=karpenter-local \
go test -v ./tests/e2e -run 'TestKarpenterLocal_' -count=1
```

### Deterministic fake price scenarios (test-only)

```bash
SPOTVORTEX_E2E_SUITE=karpenter-local \
SPOTVORTEX_TEST_PRICE_PROVIDER_FILE=tests/e2e/manifests/fake-price-scenarios.json \
go test -v ./tests/e2e -run 'TestKarpenterLocal_FakePriceProvider_' -count=1
```

## Project Status

- Karpenter local E2E coverage is active and used in CI.
- EKS Anywhere + Cluster Autoscaler exists as a dedicated harness and is currently manual-run due to environment variability.

## Open Areas for Contribution

- EKS Anywhere Docker control-plane startup hardening.
- Additional E2E suites (Managed Nodegroups, broader autoscaler paths).
- More fault-injection coverage around drain and patch failures.
- Docs and runbooks for production rollout patterns.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

If you want to contribute, open an issue with one of:

- a concrete bug report,
- a reproducible test gap,
- or a proposed design change with expected behavior.

## Security

See [SECURITY.md](SECURITY.md) and IAM references in `docs/IAM_PERMISSIONS.md`.

## License

SpotVortex Agent is licensed under Apache-2.0. See [LICENSE](LICENSE).
