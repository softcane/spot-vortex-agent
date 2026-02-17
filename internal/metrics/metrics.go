// Package metrics provides Prometheus metrics for SpotVortex.
// All metrics per phase.md lines 370-374, 785-791.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// CapacityScore tracks the TFT-predicted capacity score per node.
	// Per phase.md: spotvortex_capacity_score{node, zone}
	CapacityScore = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "capacity_score",
			Help:      "TFT-predicted eviction risk score (0=safe, 1=imminent)",
		},
		[]string{"node", "zone"},
	)

	// ActionTaken counts RL actions executed.
	// Per phase.md: spotvortex_action_taken{action}
	ActionTaken = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "action_taken_total",
			Help:      "Total number of RL actions executed",
		},
		[]string{"action"},
	)

	// SpotPriceUSD tracks current spot price per instance type and zone.
	// Per phase.md: spotvortex_spot_price_usd{instance, zone}
	SpotPriceUSD = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "spot_price_usd",
			Help:      "Current spot price in USD per hour",
		},
		[]string{"instance", "zone"},
	)

	// OnDemandPriceUSD tracks on-demand price per instance type.
	OnDemandPriceUSD = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "ondemand_price_usd",
			Help:      "On-demand price in USD per hour",
		},
		[]string{"instance"},
	)

	// SavingsUSDHourly tracks current hourly savings from spot usage.
	// Per phase.md: spotvortex_savings_usd_hourly
	SavingsUSDHourly = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "savings_usd_hourly",
			Help:      "Current hourly savings from spot vs on-demand",
		},
	)

	// OutagesAvoided counts total outages prevented by proactive migration.
	// Per phase.md: spotvortex_outages_avoided_total
	OutagesAvoided = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "outages_avoided_total",
			Help:      "Total number of spot evictions avoided",
		},
	)

	// GuardrailBlocked counts actions blocked by guardrails.
	GuardrailBlocked = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "guardrail_blocked_total",
			Help:      "Actions blocked by guardrails",
		},
		[]string{"guardrail"},
	)

	// InferenceLatency tracks ONNX inference duration.
	InferenceLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "spotvortex",
			Name:      "inference_latency_seconds",
			Help:      "Latency of ONNX model inference",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"model"}, // "tft" or "rl"
	)

	// ReconcileLoopDuration tracks the 5-minute event loop cycle time.
	ReconcileLoopDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "spotvortex",
			Name:      "reconcile_loop_duration_seconds",
			Help:      "Duration of complete reconciliation loop",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	// NodesManaged tracks number of spot nodes under management.
	NodesManaged = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "nodes_managed",
			Help:      "Number of spot nodes currently managed",
		},
	)

	// NodesDraining tracks nodes currently being drained.
	NodesDraining = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "nodes_draining",
			Help:      "Number of nodes currently draining",
		},
	)

	// MarketVolatility tracks price volatility by zone.
	MarketVolatility = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "market_volatility",
			Help:      "Rolling price volatility (std dev)",
		},
		[]string{"zone"},
	)

	// --- Dry-Run / Value Preview Metrics ---

	// PotentialSavingsHourly tracks potential hourly savings if recommended actions were taken.
	// Useful for dry-run mode to show customers the value before enabling.
	PotentialSavingsHourly = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "potential_savings_hourly_usd",
			Help:      "Potential hourly savings if node was migrated to spot (USD)",
		},
		[]string{"node", "pool", "instance_type"},
	)

	// PotentialSavingsPoolTotal tracks total potential savings per pool.
	PotentialSavingsPoolTotal = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "potential_savings_pool_total_usd",
			Help:      "Total potential monthly savings for a pool (USD)",
		},
		[]string{"pool"},
	)

	// RecommendedAction tracks the recommended action for each node/pool.
	// Values: 0=HOLD, 1=DECREASE_10, 2=DECREASE_30, 3=INCREASE_10, 4=INCREASE_30, 5=EMERGENCY
	RecommendedAction = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "recommended_action",
			Help:      "Recommended action for node (0=hold, 1-2=decrease, 3-4=increase, 5=emergency)",
		},
		[]string{"node", "pool"},
	)

	// NodesOptimizable tracks the number of nodes that could benefit from migration.
	NodesOptimizable = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "nodes_optimizable",
			Help:      "Number of on-demand nodes that could be migrated to spot",
		},
		[]string{"pool"},
	)

	// DryRunCumulativeSavings tracks cumulative savings identified across all dry-run cycles.
	DryRunCumulativeSavings = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "dry_run_cumulative_savings_usd",
			Help:      "Cumulative potential savings identified in dry-run mode",
		},
	)

	// RiskScore tracks the current risk score per pool (from TFT).
	RiskScore = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "risk_score",
			Help:      "Current risk score from TFT model (0=safe, 1=high risk)",
		},
		[]string{"pool", "zone"},
	)

	// RuntimeScore tracks the current runtime risk score per pool (from TFT runtime head).
	RuntimeScore = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "runtime_score",
			Help:      "Current runtime interruption risk score from TFT model (0=safe, 1=high risk)",
		},
		[]string{"pool", "zone"},
	)

	// SpotRatioCurrent tracks the current spot ratio per pool.
	SpotRatioCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "spot_ratio_current",
			Help:      "Current ratio of spot to total nodes in pool",
		},
		[]string{"pool"},
	)

	// SpotRatioTarget tracks the target spot ratio per pool.
	SpotRatioTarget = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "spot_ratio_target",
			Help:      "Target ratio of spot to total nodes in pool",
		},
		[]string{"pool"},
	)

	// DecisionSource counts action recommendations by source policy.
	// source=rl|deterministic
	DecisionSource = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "decision_source_total",
			Help:      "Total action recommendations grouped by policy source and action",
		},
		[]string{"source", "action"},
	)

	// UnsupportedInstanceFamily counts forced on-demand fallbacks due to model scope mismatch.
	UnsupportedInstanceFamily = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "unsupported_instance_family_total",
			Help:      "Total decisions forced to on-demand because instance family is outside model scope",
		},
		[]string{"family"},
	)

	// DeterministicDecisionReason counts deterministic policy decisions by reason code.
	DeterministicDecisionReason = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "deterministic_decision_reason_total",
			Help:      "Deterministic policy decision counts grouped by reason",
		},
		[]string{"reason"},
	)

	// WorkloadCap tracks the computed workload-based spot ratio cap per pool.
	WorkloadCap = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "workload_cap",
			Help:      "Computed workload-based maximum spot ratio for pool",
		},
		[]string{"pool"},
	)

	// WorkloadOOD tracks whether a pool is out-of-distribution (1) or in-distribution (0).
	WorkloadOOD = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "workload_ood",
			Help:      "Out-of-distribution workload flag for pool (1=OOD, 0=in-range)",
		},
		[]string{"pool"},
	)

	// WorkloadOODReason counts OOD detections by feature reason.
	WorkloadOODReason = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "workload_ood_reason_total",
			Help:      "Out-of-distribution detections grouped by feature reason",
		},
		[]string{"reason"},
	)
)

// RecordSavings calculates and records current savings.
func RecordSavings(spotPrice, onDemandPrice float64, nodeCount int) {
	if onDemandPrice > 0 && spotPrice > 0 {
		savingsPerNode := onDemandPrice - spotPrice
		totalSavings := savingsPerNode * float64(nodeCount)
		SavingsUSDHourly.Set(totalSavings)
	}
}
