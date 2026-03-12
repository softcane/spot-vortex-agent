# SpotVortex Agent

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/softcane/spot-vortex-agent)](https://goreportcard.com/report/github.com/softcane/spot-vortex-agent)

SpotVortex Agent is the in-cluster controller for safe Spot adoption.

It helps SRE and FinOps teams increase Spot usage at the node-pool level without handing workload data to an external service. The agent uses the shipped TFT risk model, live pool safety signals, and a deterministic control policy to decide when to grow, hold, freeze, or reduce Spot exposure.

![Example monthly savings for m5.2xlarge](docs/assets/deterministic_monthly_uplift_example.svg)

The chart above is a worked benchmark example using the current shipped runtime posture: `10` minute control cadence, deterministic active policy, transition-aware TFT, and `max_spot_ratio=1.0`.

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

## Reference Economics: One `m5.2xlarge` Node Over One Month

This section converts the latest offline benchmark month for the `m5.2xlarge` slice into simple unit economics.

In that benchmark month, the shipped deterministic policy:

- kept effective Spot residency at `79.05%`
- recorded `0` outages
- migrated `22` times
- used the transition-aware TFT as the market hazard signal

That gives us a simple monthly cost model:

```text
monthly_system_cost = 730 * (spot_residency * spot_rate + (1 - spot_residency) * baseline_rate)
monthly_savings = 730 * spot_residency * (baseline_rate - spot_rate)
```

This example is useful because it is easy to audit:

- one node type
- one clear Spot residency number
- one clear monthly cost formula
- three baseline cases that FinOps teams already understand

| Baseline | Baseline Rate | Spot Rate | Baseline Monthly Cost / Node | SpotVortex Monthly Cost / Node | Savings / Node / Month | Savings At 100 Nodes / Month |
|---|---:|---:|---:|---:|---:|---:|
| On-Demand | `$0.384/hr` | `$0.142/hr` | `$280.32` | `$140.67` | `$139.65` | `$13,964.74` |
| 1-Year Reserved | `$0.242/hr` | `$0.142/hr` | `$176.66` | `$118.95` | `$57.71` | `$5,770.55` |
| 3-Year Reserved | `$0.166/hr` | `$0.142/hr` | `$121.18` | `$107.33` | `$13.85` | `$1,384.93` |

These are gross compute-rate savings. Realized finance impact depends on your marginal baseline, commitment utilization, and whether the spend you move to Spot is actually displaced.


### Assumptions

- benchmark source: latest offline deterministic benchmark on the `m5.2xlarge` slice from the ML repo
- effective Spot residency: `79.05%`
- outages: `0`
- migrations: `22`
- month length: `730` hours
- Spot rate and baseline rates are illustrative example rates
- actual realized finance savings depend on whether commitment-covered spend is truly displaced

### How To Use This

- replace the example baseline with your real marginal cost
- use the benchmark table to size upside before rollout
- validate realized results from live telemetry once the controller is running in your cluster

Typical baselines:

- On-Demand
- Reserved or Savings Plan effective marginal rate
- internal chargeback rate

## How The Runtime Thinks About Risk

The runtime thinks in terms of node-pool blast radius and recovery safety. It carries a pool-safety vector for each node pool, including:

- `critical_service_spot_concentration`: share of critical-service pods in the pool that are currently running on Spot.
- `min_pdb_slack_if_one_node_lost`: worst remaining PDB slack if the pool loses its riskiest single node.
- `min_pdb_slack_if_two_nodes_lost`: worst remaining PDB slack if the pool loses its two riskiest node placements.
- `stateful_pod_fraction`: share of workload pods in the pool that are owned by StatefulSets.
- `restart_p95_seconds`: pool-level P95 restart or recovery proxy in seconds.
- `recovery_budget_violation_risk`: normalized risk that a Spot loss would push the pool outside its recovery budget.
- `spare_od_headroom_nodes`: estimated immediate On-Demand headroom still available in the pool.
- `zone_diversification_score`: score for how well the pool is spread across zones.
- `evictable_pod_fraction`: share of workload pods that are currently safe to evict voluntarily.
- `safe_max_spot_ratio`: the maximum Spot ratio the deterministic policy considers safe for the pool right now.

Today these fields are computed locally from pods, PDBs, node labels, and pool utilization:

- `critical_service_spot_concentration` = `critical pods on Spot / total critical pods`; a pod is treated as critical if `spotvortex.io/critical=true` or its priority score is `>= 0.75`.
- `min_pdb_slack_if_one_node_lost` = minimum across matching PDBs of `disruptionsAllowed - max pods from that PDB on any one node`.
- `min_pdb_slack_if_two_nodes_lost` = minimum across matching PDBs of `disruptionsAllowed - (pods on densest node + pods on second-densest node)`.
- `stateful_pod_fraction` = `StatefulSet-owned workload pods / workload pods`; workload pods exclude DaemonSets.
- `restart_p95_seconds` = CPU-weighted P95 of startup-to-ready latency, with `spotvortex.io/startup-time` as an override when present.
- `recovery_budget_violation_risk` = max heuristic signal from low or negative PDB slack, high critical-pod Spot concentration, high stateful fraction, slow restart P95, low On-Demand headroom, weak zone spread, and low evictable fraction.
- `spare_od_headroom_nodes` = `on_demand_nodes * (1 - pool_utilization)` using the runtime’s current pool utilization feed.
- `zone_diversification_score` = `0.0` for one zone, `0.5` for two zones, `1.0` for three or more zones.
- `evictable_pod_fraction` = `currently voluntary-evictable workload pods / workload pods`; a pod counts as evictable when no matching PDB blocks it, or when `disruptionsAllowed > 0`.
- `safe_max_spot_ratio` = the minimum cap produced by deterministic thresholds over the safety vector above, tightening as blast radius or recovery risk increases.

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
- production outcomes are confirmed with live telemetry

## What To Validate In Your Cluster

Before calling a rollout successful, verify:

- the agent starts and loads the bundle cleanly
- the deterministic controller is making pool-level decisions
- no unexpected drain or restart behavior appears
- interruption and recovery behavior stays acceptable
- savings improve without creating service impact

The `m5.2xlarge` example is for planning and sizing. Live telemetry is what confirms production results.

## Running Locally

```bash
helm upgrade --install spotvortex charts/spotvortex --namespace spotvortex --create-namespace
```

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
- savings examples should be expressed against a clear baseline rate
