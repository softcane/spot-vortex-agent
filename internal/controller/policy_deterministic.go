package controller

import (
	"math"
	"strings"

	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
)

// Priority score thresholds for workload spot cap calculation.
// These map from PriorityClass scoring (0-1) to max spot ratio.
const (
	priorityCriticalThreshold = 0.90 // P0/system-critical: max 20% spot
	priorityHighThreshold     = 0.70 // P1/high-priority: max 50% spot
	priorityStandardThreshold = 0.45 // P2/standard: max 80% spot
)

// deterministicDecision captures the reasoning behind a policy decision.
// Exported for metrics and observability.
type deterministicDecision struct {
	Reason        string
	CompositeRisk float64
	WorkloadCap   float64
	EffectiveCap  float64
	IsOOD         bool
	OODReasons    []string
}

// capRule maps a feature threshold to a maximum spot ratio cap.
// Rules are evaluated in order; first match wins (thresholds should be descending).
type capRule struct {
	threshold float64
	maxCap    float64
}

// PolicyEvaluator encapsulates deterministic policy evaluation logic.
// It replaces the RL model's action selection with rule-based decisions
// driven by risk scores, workload characteristics, and economics.
//
// Usage:
//
//	evaluator := NewPolicyEvaluator(runtimeCfg)
//	action, decision := evaluator.Evaluate(state, capacityScore, runtimeScore)
type PolicyEvaluator struct {
	cfg *config.RuntimeConfig

	// Cap rules for workload features. Configurable for testing.
	OutageCapRules      []capRule
	StartupCapRules     []capRule
	MigrationCapRules   []capRule
	UtilizationCapRules []capRule
}

// NewPolicyEvaluator creates a policy evaluator with default cap rules.
// Pass nil runtimeCfg to use defaults.
func NewPolicyEvaluator(runtimeCfg *config.RuntimeConfig) *PolicyEvaluator {
	cfg := config.DefaultRuntimeConfig()
	if runtimeCfg != nil {
		cfg = runtimeCfg
	}

	return &PolicyEvaluator{
		cfg: cfg,
		OutageCapRules: []capRule{
			{threshold: 96.0, maxCap: 0.10}, // 96h+ outage penalty → 10% spot max
			{threshold: 48.0, maxCap: 0.20}, // 48h+ → 20%
			{threshold: 24.0, maxCap: 0.30}, // 24h+ → 30%
			{threshold: 10.0, maxCap: 0.50}, // 10h+ → 50%
		},
		StartupCapRules: []capRule{
			{threshold: 600.0, maxCap: 0.20}, // 10min+ startup → 20% spot max
			{threshold: 300.0, maxCap: 0.30}, // 5min+ → 30%
			{threshold: 120.0, maxCap: 0.50}, // 2min+ → 50%
		},
		MigrationCapRules: []capRule{
			{threshold: 8.0, maxCap: 0.20}, // $8+ migration cost → 20% spot max
			{threshold: 5.0, maxCap: 0.30}, // $5+ → 30%
			{threshold: 2.0, maxCap: 0.60}, // $2+ → 60%
		},
		UtilizationCapRules: []capRule{
			{threshold: 0.95, maxCap: 0.70}, // 95%+ utilization → 70% spot max
		},
	}
}

// Evaluate runs the deterministic policy and returns an action with reasoning.
//
// Decision order:
//  1. Emergency risk → EMERGENCY_EXIT
//  2. High risk → DECREASE_30
//  3. Medium risk → DECREASE_10
//  4. Cap reached → HOLD
//  5. OOD conservative → cautious INCREASE_10 or HOLD
//  6. In-distribution economic → INCREASE_30/INCREASE_10 if economics qualify
//  7. Default → HOLD
func (p *PolicyEvaluator) Evaluate(
	state inference.NodeState,
	capacityScore float64,
	runtimeScore float64,
) (inference.Action, deterministicDecision) {
	dp := p.cfg.DeterministicPolicy

	compositeRisk := math.Max(capacityScore, runtimeScore)
	workloadCap := p.computeWorkloadSpotCap(state)
	effectiveCap := clampRange(workloadCap, p.cfg.MinSpotRatio, p.cfg.MaxSpotRatio)

	isOOD, oodReasons := detectOOD(state, dp.FeatureBuckets)
	decision := deterministicDecision{
		CompositeRisk: compositeRisk,
		WorkloadCap:   workloadCap,
		EffectiveCap:  effectiveCap,
		IsOOD:         isOOD,
		OODReasons:    oodReasons,
	}

	// 1. Emergency: composite risk or runtime score exceeds emergency thresholds
	if compositeRisk >= dp.EmergencyRiskThreshold || runtimeScore >= dp.RuntimeEmergencyThreshold {
		decision.Reason = "emergency_risk"
		return inference.ActionEmergencyExit, decision
	}

	// 2. High risk
	if compositeRisk >= dp.HighRiskThreshold {
		decision.Reason = "high_risk"
		return inference.ActionDecrease30, decision
	}

	// 3. Medium risk
	if compositeRisk >= dp.MediumRiskThreshold {
		decision.Reason = "medium_risk"
		return inference.ActionDecrease10, decision
	}

	// 4. Already at cap
	if state.CurrentSpotRatio >= effectiveCap {
		decision.Reason = "cap_reached"
		return inference.ActionHold, decision
	}

	// 5. Out-of-distribution: be conservative
	if isOOD && strings.EqualFold(dp.OODMode, "conservative") {
		if canIncreaseSpot(state, compositeRisk, dp.OODMaxRiskForIncrease, dp.OODMinSavingsRatioForIncrease, dp.OODMaxPaybackHoursForIncrease) {
			decision.Reason = "ood_conservative_increase10"
			return inference.ActionIncrease10, decision
		}
		decision.Reason = "ood_conservative_hold"
		return inference.ActionHold, decision
	}

	// 6. In-distribution: economic analysis for increase
	if canIncreaseSpot(state, compositeRisk, dp.MediumRiskThreshold, dp.MinSavingsRatioForIncrease, dp.MaxPaybackHoursForIncrease) {
		if effectiveCap-state.CurrentSpotRatio >= 0.25 {
			decision.Reason = "economic_increase30"
			return inference.ActionIncrease30, decision
		}
		decision.Reason = "economic_increase10"
		return inference.ActionIncrease10, decision
	}

	// 7. Default: hold
	decision.Reason = "hold_no_edge"
	return inference.ActionHold, decision
}

// computeWorkloadSpotCap calculates the maximum safe spot ratio based on workload features.
// Each feature independently constrains the cap; the minimum across all constraints wins.
func (p *PolicyEvaluator) computeWorkloadSpotCap(state inference.NodeState) float64 {
	cap := 1.0

	// Priority-based cap
	switch {
	case state.PriorityScore >= priorityCriticalThreshold:
		cap = math.Min(cap, 0.20)
	case state.PriorityScore >= priorityHighThreshold:
		cap = math.Min(cap, 0.50)
	case state.PriorityScore >= priorityStandardThreshold:
		cap = math.Min(cap, 0.80)
	}

	// Feature-based caps (each independently constrains)
	cap = applyCapRules(state.OutagePenaltyHours, cap, p.OutageCapRules)
	cap = applyCapRules(state.PodStartupTime, cap, p.StartupCapRules)
	cap = applyCapRules(state.MigrationCost, cap, p.MigrationCapRules)
	cap = applyCapRules(state.ClusterUtilization, cap, p.UtilizationCapRules)

	return clamp01(cap)
}

// evaluateDeterministicPolicy is the backward-compatible entry point used by controller.go.
// It creates a PolicyEvaluator and delegates to it.
func evaluateDeterministicPolicy(
	state inference.NodeState,
	capacityScore float64,
	runtimeScore float64,
	runtimeCfg *config.RuntimeConfig,
) (inference.Action, deterministicDecision) {
	evaluator := NewPolicyEvaluator(runtimeCfg)
	return evaluator.Evaluate(state, capacityScore, runtimeScore)
}

// canIncreaseSpot checks if economics justify increasing spot ratio.
// Requires: low risk, sufficient savings ratio, and acceptable payback period.
func canIncreaseSpot(
	state inference.NodeState,
	compositeRisk float64,
	maxRisk float64,
	minSavingsRatio float64,
	maxPaybackHours float64,
) bool {
	if state.OnDemandPrice <= 0 {
		return false
	}
	delta := math.Max(0.0, state.OnDemandPrice-state.SpotPrice)
	if delta <= 0 {
		return false
	}
	savingsRatio := delta / math.Max(state.OnDemandPrice, 1e-6)
	paybackHours := state.MigrationCost / math.Max(delta, 1e-6)

	return compositeRisk <= maxRisk &&
		savingsRatio >= minSavingsRatio &&
		paybackHours <= maxPaybackHours
}

func applyCapRules(value float64, currentCap float64, rules []capRule) float64 {
	for _, rule := range rules {
		if value >= rule.threshold {
			return math.Min(currentCap, rule.maxCap)
		}
	}
	return currentCap
}

func detectOOD(state inference.NodeState, buckets config.FeatureBuckets) (bool, []string) {
	reasons := make([]string, 0, 4)
	if outOfRange(state.PodStartupTime, buckets.PodStartupTimeSeconds) {
		reasons = append(reasons, "pod_startup_time")
	}
	if outOfRange(state.OutagePenaltyHours, buckets.OutagePenaltyHours) {
		reasons = append(reasons, "outage_penalty_hours")
	}
	if outOfRange(state.PriorityScore, buckets.PriorityScore) {
		reasons = append(reasons, "priority_score")
	}
	if outOfRange(state.ClusterUtilization, buckets.ClusterUtilization) {
		reasons = append(reasons, "cluster_utilization")
	}
	return len(reasons) > 0, reasons
}

func outOfRange(value float64, boundaries []float64) bool {
	if len(boundaries) < 2 {
		return false
	}
	return value < boundaries[0] || value > boundaries[len(boundaries)-1]
}

func clamp01(v float64) float64 {
	return clampRange(v, 0, 1)
}

func clampRange(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
