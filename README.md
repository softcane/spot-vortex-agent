# SpotVortex Agent

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/softcane/spot-vortex-agent)](https://goreportcard.com/report/github.com/softcane/spot-vortex-agent)

SpotVortex Agent is the in-cluster controller for safe Spot adoption.

It helps SRE and FinOps teams increase Spot usage at the node-pool level without handing workload data to an external service. The agent uses the shipped TFT risk model, live pool safety signals, and a deterministic control policy to decide when to grow, hold, freeze, or reduce Spot exposure.

![Example simulated monthly deterministic uplift](docs/assets/deterministic_monthly_uplift_example.svg)

The chart above is a simulated example, not a production guarantee. It shows the current shipped runtime posture: `10` minute control cadence, deterministic active policy, transition-aware TFT, and `max_spot_ratio=1.0`.

## What You Deploy Today

- Active policy mode: `deterministic`
- Control cadence: `10` minutes
- Spot bounds: `min_spot_ratio=0.167`, `max_spot_ratio=1.0`
- Preferred operating point: `target_spot_ratio=0.5`
- Market hazard model: transition-aware TFT from `models/tft.onnx`
- RL: shadow-only; it records comparison telemetry and does not actuate production changes
- Bundle contract: `models/MODEL_MANIFEST.json`
- Cloud scope: AWS, `60` supported instance families

The shipped runtime config lives in [config/runtime.json](config/runtime.json).

## How It Works

1. Loads the local ONNX bundle from `models/`.
2. Uses TFT as the market risk signal.
3. Builds a pool-safety view from live cluster state.
4. Chooses a deterministic response for each node pool.
5. Applies that response with steering, tainting, draining, and replacement controls.
6. Keeps RL in shadow for comparison only.

The control unit is the node pool, not the individual pod.

## How To Read The Savings Chart

- It is a simulated monthly net uplift example.
- It is scaled from the latest offline deterministic scorecard.
- It assumes a `730` hour month.
- It assumes AWS pools similar to `c6i.large`, `m6i.large`, and `r6i.large`.
- It compares the current deterministic runtime to the older deterministic runtime.
- It is not a promise for your production cluster.

If you are a FinOps or SRE reader, the right takeaway is:

- this is the direction and size of the value we are targeting
- real results depend on your workload mix, pool safety, and interruption conditions
- production rollout quality must be judged from live telemetry, not from this chart alone

## How The Runtime Thinks About Risk

The runtime does not only use average workload severity. It also carries a pool-safety vector that approximates blast radius for each node pool, including:

- `critical_service_spot_concentration`
- `min_pdb_slack_if_one_node_lost`
- `min_pdb_slack_if_two_nodes_lost`
- `stateful_pod_fraction`
- `restart_p95_seconds`
- `recovery_budget_violation_risk`
- `spare_od_headroom_nodes`
- `zone_diversification_score`
- `evictable_pod_fraction`
- `safe_max_spot_ratio`

This keeps the decision node-level while making the policy more sensitive to real service impact.

If some pool-safety signals are unavailable, the runtime falls back to safe deterministic defaults instead of silently promoting RL behavior.

## How To Roll It Out

Treat SpotVortex as an operational control system, not just a model bundle.

Recommended rollout path:

1. Install the agent in dry-run or shadow mode first.
2. Confirm the bundle loads and the controller runs cleanly.
3. Check the agent’s recommendations against your current pool behavior.
4. Start with a limited set of node pools.
5. Watch live interruption, drain, restart, and recovery telemetry.
6. Expand only after the cluster behavior is stable.

The runtime facts that matter for rollout are:

- the shipped controller is deterministic
- TFT is the live market risk input
- RL is shadow telemetry only
- bundle checksums are enforced through `models/MODEL_MANIFEST.json`
- production value has to be confirmed with live telemetry

## What To Validate In Your Cluster

Before calling a rollout successful, verify:

- the agent starts and loads the bundle cleanly
- the deterministic controller is making pool-level decisions
- no unexpected drain or restart behavior appears
- interruption and recovery behavior stays acceptable
- savings improve without creating service impact

The savings chart is useful for planning. Live telemetry is what decides production success.

## Quick Validation

Focused runtime validation:

```bash
go test ./internal/config/...
go test ./internal/inference/...
go test ./internal/controller/...
```

Deterministic Kind end-to-end path:

```bash
go test -v ./internal/controller -run TestDeterministicModeKindInferencePath -count=1
```

Release/install proof on Kind:

```bash
bash hack/verify-release-kind-install.sh
```

## Running Locally

The agent expects:

- Kubernetes access
- ONNX runtime library available to the process
- model bundle in `models/`
- runtime config in `config/runtime.json`

Local run path:

```bash
go run ./cmd/agent run
```

For shadow-style local testing, use dry-run deployment settings rather than editing the shipped runtime policy contract.


## Repository Layout

- [cmd/](cmd/): CLI entrypoints
- [config/](config/): shipped runtime config and install defaults
- [internal/config/](internal/config/): runtime config schema and normalization
- [internal/controller/](internal/controller/): deterministic controller and execution logic
- [internal/inference/](internal/inference/): ONNX bundle loading and inference contract
- [models/](models/): shipped model bundle and manifest
- [tests/e2e/](tests/e2e/): Kind and install-path helpers
- [hack/](hack/): release and install verification scripts

## Current Runtime Facts

- deterministic is the active runtime path
- TFT is the shipped market model
- RL is shadow-only
- `10` minutes is the active cadence
- manifest-verified bundle loading is required
- simulated value claims must be labeled as simulated
