package controller

import (
	"math"
	"strings"

	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
)

// PolicyResponseMode is the runtime intent emitted by deterministic policy.
// Node taint, drain, and replacement remain execution tools that map from this
// intent onto today's discrete actions.
type PolicyResponseMode string

const (
	ResponseModeAllowGrowth       PolicyResponseMode = "allow_growth"
	ResponseModeHold              PolicyResponseMode = "hold"
	ResponseModeFreezeSpot        PolicyResponseMode = "freeze_spot"
	ResponseModeReduceSpotGradual PolicyResponseMode = "reduce_spot_gradual"
	ResponseModeReduceSpotFast    PolicyResponseMode = "reduce_spot_fast"
	ResponseModeEmergencyExit     PolicyResponseMode = "emergency_exit"
)

// PolicyUrgency is the qualitative urgency attached to a response intent.
type PolicyUrgency string

const (
	PolicyUrgencyLow      PolicyUrgency = "low"
	PolicyUrgencyMedium   PolicyUrgency = "medium"
	PolicyUrgencyHigh     PolicyUrgency = "high"
	PolicyUrgencyCritical PolicyUrgency = "critical"
)

// deterministicDecision captures the reasoning behind a policy decision.
// Exported for metrics and observability.
type deterministicDecision struct {
	Reason        string
	ResponseMode  PolicyResponseMode
	Urgency       PolicyUrgency
	CompositeRisk float64
	FeatureCap    float64
	WorkloadCap   float64
	EffectiveCap  float64
	PoolSafety    config.PoolSafetyVector
	IsOOD         bool
	OODReasons    []string
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

	// Cap rules for workload features. Resolved from runtime config for parity.
	PriorityCapRules    []config.SpotRatioCapRule
	OutageCapRules      []config.SpotRatioCapRule
	StartupCapRules     []config.SpotRatioCapRule
	MigrationCapRules   []config.SpotRatioCapRule
	UtilizationCapRules []config.SpotRatioCapRule
}

// NewPolicyEvaluator creates a policy evaluator with config-driven cap rules.
// Pass nil runtimeCfg to use default runtime config values.
func NewPolicyEvaluator(runtimeCfg *config.RuntimeConfig) *PolicyEvaluator {
	cfg := config.DefaultRuntimeConfig()
	if runtimeCfg != nil {
		cfg = runtimeCfg
	}
	dp := cfg.DeterministicPolicy

	return &PolicyEvaluator{
		cfg:                 cfg,
		PriorityCapRules:    dp.ResolvedPriorityCapRules(),
		OutageCapRules:      dp.ResolvedOutagePenaltyCapRules(),
		StartupCapRules:     dp.ResolvedStartupTimeCapRules(),
		MigrationCapRules:   dp.ResolvedMigrationCostCapRules(),
		UtilizationCapRules: dp.ResolvedUtilizationCapRules(),
	}
}

// Evaluate runs the deterministic policy and returns an action with reasoning.
//
// Decision order:
//  1. Market emergency risk → emergency_exit
//  2. Market high risk → reduce_spot_fast
//  3. Market medium risk → reduce_spot_gradual
//  4. Pool safety overshoot → reduce_spot_fast or reduce_spot_gradual
//  5. Pool safety freeze → freeze_spot
//  6. OOD conservative → cautious allow_growth or hold
//  7. In-distribution economic edge → allow_growth
//  8. Default → hold
func (p *PolicyEvaluator) Evaluate(
	state inference.NodeState,
	capacityScore float64,
	runtimeScore float64,
) (inference.Action, deterministicDecision) {
	dp := p.cfg.DeterministicPolicy

	compositeRisk := math.Max(capacityScore, runtimeScore)
	workloadCap, featureCap, poolSafety := p.resolveWorkloadSurface(state)
	effectiveCap := clampRange(workloadCap, p.cfg.MinSpotRatio, p.cfg.MaxSpotRatio)

	isOOD, oodReasons := detectOOD(state, dp.FeatureBuckets)
	decision := deterministicDecision{
		CompositeRisk: compositeRisk,
		FeatureCap:    featureCap,
		WorkloadCap:   workloadCap,
		EffectiveCap:  effectiveCap,
		PoolSafety:    poolSafety,
		IsOOD:         isOOD,
		OODReasons:    oodReasons,
	}

	// 1. Emergency: composite risk or runtime score exceeds emergency thresholds
	if compositeRisk >= dp.EmergencyRiskThreshold || runtimeScore >= dp.RuntimeEmergencyThreshold {
		decision.Reason = "emergency_risk"
		decision.ResponseMode = ResponseModeEmergencyExit
		decision.Urgency = PolicyUrgencyCritical
		return inference.ActionEmergencyExit, decision
	}

	// 2. High risk
	if compositeRisk >= dp.HighRiskThreshold {
		decision.Reason = "high_risk"
		decision.ResponseMode = ResponseModeReduceSpotFast
		decision.Urgency = PolicyUrgencyHigh
		return inference.ActionDecrease30, decision
	}

	// 3. Medium risk
	if compositeRisk >= dp.MediumRiskThreshold {
		decision.Reason = "medium_risk"
		decision.ResponseMode = ResponseModeReduceSpotGradual
		decision.Urgency = PolicyUrgencyMedium
		return inference.ActionDecrease10, decision
	}

	overshoot := state.CurrentSpotRatio - effectiveCap
	if overshoot > 1e-6 {
		if overshoot >= 0.25 || poolSafety.RecoveryBudgetViolationRisk >= 0.85 || poolSafety.MinPDBSlackIfOneNodeLost < 0 {
			decision.Reason = "pool_safety_reduce_fast"
			decision.ResponseMode = ResponseModeReduceSpotFast
			decision.Urgency = PolicyUrgencyHigh
			return inference.ActionDecrease30, decision
		}
		decision.Reason = "pool_safety_reduce_gradual"
		decision.ResponseMode = ResponseModeReduceSpotGradual
		decision.Urgency = PolicyUrgencyMedium
		return inference.ActionDecrease10, decision
	}

	// 5. Already at cap or safety says do not grow.
	if state.CurrentSpotRatio >= effectiveCap-1e-6 {
		decision.Reason = "cap_reached"
		decision.ResponseMode = ResponseModeFreezeSpot
		decision.Urgency = PolicyUrgencyMedium
		return inference.ActionHold, decision
	}
	if poolSafety.RecoveryBudgetViolationRisk >= 0.60 {
		decision.Reason = "pool_safety_freeze"
		decision.ResponseMode = ResponseModeFreezeSpot
		decision.Urgency = PolicyUrgencyMedium
		return inference.ActionHold, decision
	}

	// 6. Out-of-distribution: be conservative
	if isOOD && strings.EqualFold(dp.OODMode, "conservative") {
		if canIncreaseSpot(state, compositeRisk, dp.OODMaxRiskForIncrease, dp.OODMinSavingsRatioForIncrease, dp.OODMaxPaybackHoursForIncrease) {
			decision.Reason = "ood_conservative_increase10"
			decision.ResponseMode = ResponseModeAllowGrowth
			decision.Urgency = PolicyUrgencyLow
			return inference.ActionIncrease10, decision
		}
		decision.Reason = "ood_conservative_hold"
		decision.ResponseMode = ResponseModeHold
		decision.Urgency = PolicyUrgencyLow
		return inference.ActionHold, decision
	}

	// 7. In-distribution: economic analysis for increase
	if canIncreaseSpot(state, compositeRisk, dp.MediumRiskThreshold, dp.MinSavingsRatioForIncrease, dp.MaxPaybackHoursForIncrease) {
		decision.ResponseMode = ResponseModeAllowGrowth
		decision.Urgency = PolicyUrgencyLow
		if effectiveCap-state.CurrentSpotRatio >= 0.25 {
			decision.Reason = "economic_increase30"
			return inference.ActionIncrease30, decision
		}
		decision.Reason = "economic_increase10"
		return inference.ActionIncrease10, decision
	}

	// 8. Default: hold
	decision.Reason = "hold_no_edge"
	decision.ResponseMode = ResponseModeHold
	decision.Urgency = PolicyUrgencyLow
	return inference.ActionHold, decision
}

func (p *PolicyEvaluator) resolveWorkloadSurface(state inference.NodeState) (float64, float64, config.PoolSafetyVector) {
	featureCap := p.computeFeatureSpotCap(state)
	poolSafety := resolvePoolSafetyVector(state.PoolSafety)
	workloadCap := math.Min(featureCap, poolSafety.SafeMaxSpotRatio)
	return clamp01(workloadCap), featureCap, poolSafety
}

// computeWorkloadSpotCap calculates the maximum safe spot ratio from the
// feature-rule cap and the Phase 1 pool safety vector. The minimum wins.
func (p *PolicyEvaluator) computeWorkloadSpotCap(state inference.NodeState) float64 {
	workloadCap, _, _ := p.resolveWorkloadSurface(state)
	return workloadCap
}

// computeFeatureSpotCap derives the feature-rule cap from the configured
// severity and economics surfaces before pool-safety tightening is applied.
func (p *PolicyEvaluator) computeFeatureSpotCap(state inference.NodeState) float64 {
	cap := 1.0

	// Feature-based caps (each independently constrains)
	cap = applyCapRules(state.PriorityScore, cap, p.PriorityCapRules)
	cap = applyCapRules(state.OutagePenaltyHours, cap, p.OutageCapRules)
	cap = applyCapRules(state.PodStartupTime, cap, p.StartupCapRules)
	cap = applyCapRules(state.MigrationCost, cap, p.MigrationCapRules)
	cap = applyCapRules(state.ClusterUtilization, cap, p.UtilizationCapRules)

	return clamp01(cap)
}

func resolvePoolSafetyVector(v config.PoolSafetyVector) config.PoolSafetyVector {
	if v.IsZero() {
		return config.DefaultPoolSafetyVector()
	}
	return config.NormalizePoolSafetyVector(v)
}

// evaluateDeterministicPolicy creates a PolicyEvaluator and delegates to it.
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

func applyCapRules(value float64, currentCap float64, rules []config.SpotRatioCapRule) float64 {
	for _, rule := range rules {
		if value >= rule.Threshold {
			return math.Min(currentCap, rule.MaxSpotRatio)
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
