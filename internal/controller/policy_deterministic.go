package controller

import (
	"math"
	"strings"

	"github.com/pradeepsingh/spot-vortex-agent/internal/config"
	"github.com/pradeepsingh/spot-vortex-agent/internal/inference"
)

type deterministicDecision struct {
	Reason        string
	CompositeRisk float64
	WorkloadCap   float64
	EffectiveCap  float64
	IsOOD         bool
	OODReasons    []string
}

type capRule struct {
	threshold float64
	maxCap    float64
}

var (
	outageCapRules = []capRule{
		{threshold: 96.0, maxCap: 0.10},
		{threshold: 48.0, maxCap: 0.20},
		{threshold: 24.0, maxCap: 0.30},
		{threshold: 10.0, maxCap: 0.50},
	}
	startupCapRules = []capRule{
		{threshold: 600.0, maxCap: 0.20},
		{threshold: 300.0, maxCap: 0.30},
		{threshold: 120.0, maxCap: 0.50},
	}
	migrationCapRules = []capRule{
		{threshold: 8.0, maxCap: 0.20},
		{threshold: 5.0, maxCap: 0.30},
		{threshold: 2.0, maxCap: 0.60},
	}
	utilizationCapRules = []capRule{
		{threshold: 0.95, maxCap: 0.70},
	}
)

func evaluateDeterministicPolicy(
	state inference.NodeState,
	capacityScore float64,
	runtimeScore float64,
	runtimeCfg *config.RuntimeConfig,
) (inference.Action, deterministicDecision) {
	cfg := config.DefaultRuntimeConfig()
	if runtimeCfg != nil {
		cfg = runtimeCfg
	}
	dp := cfg.DeterministicPolicy

	compositeRisk := math.Max(capacityScore, runtimeScore)
	workloadCap := computeWorkloadSpotCap(state)
	effectiveCap := math.Min(workloadCap, cfg.MaxSpotRatio)
	effectiveCap = math.Max(effectiveCap, cfg.MinSpotRatio)

	isOOD, oodReasons := detectOOD(state, dp.FeatureBuckets)
	decision := deterministicDecision{
		CompositeRisk: compositeRisk,
		WorkloadCap:   workloadCap,
		EffectiveCap:  effectiveCap,
		IsOOD:         isOOD,
		OODReasons:    oodReasons,
	}

	if compositeRisk >= dp.EmergencyRiskThreshold || runtimeScore >= dp.RuntimeEmergencyThreshold {
		decision.Reason = "emergency_risk"
		return inference.ActionEmergencyExit, decision
	}
	if compositeRisk >= dp.HighRiskThreshold {
		decision.Reason = "high_risk"
		return inference.ActionDecrease30, decision
	}
	if compositeRisk >= dp.MediumRiskThreshold {
		decision.Reason = "medium_risk"
		return inference.ActionDecrease10, decision
	}
	if state.CurrentSpotRatio >= effectiveCap {
		decision.Reason = "cap_reached"
		return inference.ActionHold, decision
	}

	if isOOD && strings.EqualFold(dp.OODMode, "conservative") {
		if canIncreaseSpot(state, compositeRisk, dp.OODMaxRiskForIncrease, dp.OODMinSavingsRatioForIncrease, dp.OODMaxPaybackHoursForIncrease) {
			decision.Reason = "ood_conservative_increase10"
			return inference.ActionIncrease10, decision
		}
		decision.Reason = "ood_conservative_hold"
		return inference.ActionHold, decision
	}

	if canIncreaseSpot(state, compositeRisk, dp.MediumRiskThreshold, dp.MinSavingsRatioForIncrease, dp.MaxPaybackHoursForIncrease) {
		if effectiveCap-state.CurrentSpotRatio >= 0.25 {
			decision.Reason = "economic_increase30"
			return inference.ActionIncrease30, decision
		}
		decision.Reason = "economic_increase10"
		return inference.ActionIncrease10, decision
	}

	decision.Reason = "hold_no_edge"
	return inference.ActionHold, decision
}

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

func computeWorkloadSpotCap(state inference.NodeState) float64 {
	cap := 1.0

	priorityCap := 1.0
	switch {
	case state.PriorityScore >= 0.90:
		priorityCap = 0.20
	case state.PriorityScore >= 0.70:
		priorityCap = 0.50
	case state.PriorityScore >= 0.45:
		priorityCap = 0.80
	}
	cap = math.Min(cap, priorityCap)

	cap = applyCapRules(state.OutagePenaltyHours, cap, outageCapRules)
	cap = applyCapRules(state.PodStartupTime, cap, startupCapRules)
	cap = applyCapRules(state.MigrationCost, cap, migrationCapRules)
	cap = applyCapRules(state.ClusterUtilization, cap, utilizationCapRules)

	return clamp01(cap)
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
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
