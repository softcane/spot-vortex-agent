package controller

import (
	"testing"

	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
)

func deterministicRuntimeConfig() *config.RuntimeConfig {
	return &config.RuntimeConfig{
		MinSpotRatio: 0.0,
		MaxSpotRatio: 1.0,
		DeterministicPolicy: config.DeterministicPolicyConfig{
			EmergencyRiskThreshold:        0.90,
			RuntimeEmergencyThreshold:     0.80,
			HighRiskThreshold:             0.60,
			MediumRiskThreshold:           0.35,
			MinSavingsRatioForIncrease:    0.15,
			MaxPaybackHoursForIncrease:    6.0,
			OODMode:                       "conservative",
			OODMaxRiskForIncrease:         0.25,
			OODMinSavingsRatioForIncrease: 0.25,
			OODMaxPaybackHoursForIncrease: 3.0,
			FeatureBuckets: config.FeatureBuckets{
				PodStartupTimeSeconds: []float64{0, 60, 120, 300, 600},
				OutagePenaltyHours:    []float64{0, 1, 4, 10, 24},
				PriorityScore:         []float64{0.0, 0.25, 0.5, 0.75, 1.0},
				ClusterUtilization:    []float64{0.0, 0.5, 0.7, 0.85, 1.0},
			},
		},
	}
}

func baseDeterministicState() inference.NodeState {
	return inference.NodeState{
		SpotPrice:          0.5,
		OnDemandPrice:      1.0,
		ClusterUtilization: 0.6,
		PodStartupTime:     30.0,
		OutagePenaltyHours: 1.0,
		MigrationCost:      0.5,
		PriorityScore:      0.20,
		CurrentSpotRatio:   0.20,
		TimeSinceMigration: 50,
		TargetSpotRatio:    0.50,
		PriceHistory:       []float64{0.5, 0.52, 0.51},
	}
}

func TestEvaluateDeterministicPolicy_EmergencyRisk(t *testing.T) {
	state := baseDeterministicState()
	action, decision := evaluateDeterministicPolicy(state, 0.95, 0.20, deterministicRuntimeConfig())
	if action != inference.ActionEmergencyExit {
		t.Fatalf("expected EMERGENCY_EXIT, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "emergency_risk" {
		t.Fatalf("expected emergency_risk reason, got %q", decision.Reason)
	}
}

func TestEvaluateDeterministicPolicy_HighAndMediumBands(t *testing.T) {
	state := baseDeterministicState()

	action, decision := evaluateDeterministicPolicy(state, 0.65, 0.20, deterministicRuntimeConfig())
	if action != inference.ActionDecrease30 {
		t.Fatalf("expected DECREASE_30 for high risk, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "high_risk" {
		t.Fatalf("expected high_risk reason, got %q", decision.Reason)
	}

	action, decision = evaluateDeterministicPolicy(state, 0.40, 0.20, deterministicRuntimeConfig())
	if action != inference.ActionDecrease10 {
		t.Fatalf("expected DECREASE_10 for medium risk, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "medium_risk" {
		t.Fatalf("expected medium_risk reason, got %q", decision.Reason)
	}
}

func TestEvaluateDeterministicPolicy_HoldWhenCapReached(t *testing.T) {
	state := baseDeterministicState()
	state.PriorityScore = 0.80 // workload cap should be <= 0.50
	state.CurrentSpotRatio = 0.60

	action, decision := evaluateDeterministicPolicy(state, 0.10, 0.10, deterministicRuntimeConfig())
	if action != inference.ActionHold {
		t.Fatalf("expected HOLD when cap reached, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "cap_reached" {
		t.Fatalf("expected cap_reached reason, got %q", decision.Reason)
	}
	if decision.EffectiveCap > 0.50 {
		t.Fatalf("expected effective cap <= 0.50, got %.2f", decision.EffectiveCap)
	}
}

func TestEvaluateDeterministicPolicy_EconomicIncrease30(t *testing.T) {
	state := baseDeterministicState()

	action, decision := evaluateDeterministicPolicy(state, 0.10, 0.10, deterministicRuntimeConfig())
	if action != inference.ActionIncrease30 {
		t.Fatalf("expected INCREASE_30, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "economic_increase30" {
		t.Fatalf("expected economic_increase30 reason, got %q", decision.Reason)
	}
}

func TestEvaluateDeterministicPolicy_OODConservative(t *testing.T) {
	state := baseDeterministicState()
	state.PodStartupTime = 4000 // OOD for configured buckets
	state.CurrentSpotRatio = 0.05

	action, decision := evaluateDeterministicPolicy(state, 0.10, 0.10, deterministicRuntimeConfig())
	if action != inference.ActionIncrease10 {
		t.Fatalf("expected OOD conservative INCREASE_10, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "ood_conservative_increase10" {
		t.Fatalf("expected ood_conservative_increase10 reason, got %q", decision.Reason)
	}
	if !decision.IsOOD {
		t.Fatal("expected OOD decision flag to be true")
	}

	state = baseDeterministicState()
	state.PodStartupTime = 4000
	state.CurrentSpotRatio = 0.05
	state.SpotPrice = 0.95 // weak savings edge
	action, decision = evaluateDeterministicPolicy(state, 0.10, 0.10, deterministicRuntimeConfig())
	if action != inference.ActionHold {
		t.Fatalf("expected OOD conservative HOLD with weak economics, got %s", inference.ActionToString(action))
	}
	if decision.Reason != "ood_conservative_hold" {
		t.Fatalf("expected ood_conservative_hold reason, got %q", decision.Reason)
	}
}

func TestComputeWorkloadSpotCap_CombinesRules(t *testing.T) {
	state := baseDeterministicState()
	state.PriorityScore = 0.95
	state.OutagePenaltyHours = 100

	evaluator := NewPolicyEvaluator(nil)
	cap := evaluator.computeWorkloadSpotCap(state)
	if cap > 0.10 {
		t.Fatalf("expected strict cap <= 0.10, got %.2f", cap)
	}
}
