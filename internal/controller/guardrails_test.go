package controller

import (
	"testing"
)

func TestCheckHighUtilization(t *testing.T) {
	// Create guardrail checker with default settings
	g := &GuardrailChecker{
		highUtilizationThreshold: 0.85,
	}

	tests := []struct {
		name           string
		utilization    float64
		action         Action
		wantApproved   bool
		wantModified   Action
		wantHasReason  bool
	}{
		{
			name:          "low utilization - approve any action",
			utilization:   0.50,
			action:        ActionDecrease30,
			wantApproved:  true,
			wantModified:  ActionDecrease30,
			wantHasReason: false,
		},
		{
			name:          "HOLD action always passes",
			utilization:   0.95,
			action:        ActionHold,
			wantApproved:  true,
			wantModified:  ActionHold,
			wantHasReason: false,
		},
		{
			name:          "INCREASE action always passes",
			utilization:   0.95,
			action:        ActionIncrease30,
			wantApproved:  true,
			wantModified:  ActionIncrease30,
			wantHasReason: false,
		},
		{
			name:          "high utilization - downgrade EMERGENCY to DECREASE_30",
			utilization:   0.90,
			action:        ActionEmergencyExit,
			wantApproved:  true,
			wantModified:  ActionDecrease30, // Downgraded
			wantHasReason: true,
		},
		{
			name:          "very high utilization (>95%) - block DECREASE actions",
			utilization:   0.96,
			action:        ActionDecrease30,
			wantApproved:  false,
			wantHasReason: true,
		},
		{
			name:          "high but not extreme (86%) - allow with warning",
			utilization:   0.86,
			action:        ActionDecrease10,
			wantApproved:  true,
			wantModified:  ActionDecrease10,
			wantHasReason: true, // Has warning reason
		},
		{
			name:          "at threshold (85%) - passes",
			utilization:   0.85,
			action:        ActionDecrease30,
			wantApproved:  true,
			wantModified:  ActionDecrease30,
			wantHasReason: false, // No warning when exactly at threshold
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := NodeState{
				ClusterUtilization: tc.utilization,
			}

			result := g.checkHighUtilization(state, tc.action)

			if result.Approved != tc.wantApproved {
				t.Errorf("Approved: got %v, want %v", result.Approved, tc.wantApproved)
			}

			if tc.wantApproved && result.ModifiedAction != tc.wantModified {
				t.Errorf("ModifiedAction: got %v, want %v", result.ModifiedAction, tc.wantModified)
			}

			hasReason := result.Reason != ""
			if hasReason != tc.wantHasReason {
				t.Errorf("HasReason: got %v, want %v (reason: %q)", hasReason, tc.wantHasReason, result.Reason)
			}

			if result.GuardrailName != "high_utilization" {
				t.Errorf("GuardrailName: got %q, want %q", result.GuardrailName, "high_utilization")
			}
		})
	}
}

func TestGuardrailCheckerDefaults(t *testing.T) {
	g := NewGuardrailChecker(nil, nil, 0)

	// Test defaults are set
	if g.clusterFractionLimit != 0.20 {
		t.Errorf("clusterFractionLimit: got %.2f, want 0.20", g.clusterFractionLimit)
	}
	if g.confidenceThreshold != 0.50 {
		t.Errorf("confidenceThreshold: got %.2f, want 0.50", g.confidenceThreshold)
	}
	if g.highUtilizationThreshold != 0.85 {
		t.Errorf("highUtilizationThreshold: got %.2f, want 0.85", g.highUtilizationThreshold)
	}
}

func TestCheckConfidence(t *testing.T) {
	g := &GuardrailChecker{
		confidenceThreshold: 0.50,
	}

	tests := []struct {
		name         string
		confidence   float64
		wantApproved bool
	}{
		{name: "above threshold", confidence: 0.75, wantApproved: true},
		{name: "at threshold", confidence: 0.50, wantApproved: true},
		{name: "below threshold", confidence: 0.49, wantApproved: false},
		{name: "zero confidence", confidence: 0.0, wantApproved: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := NodeState{Confidence: tc.confidence}
			result := g.checkConfidence(state)

			if result.Approved != tc.wantApproved {
				t.Errorf("Approved: got %v, want %v", result.Approved, tc.wantApproved)
			}

			if result.GuardrailName != "low_confidence" {
				t.Errorf("GuardrailName: got %q, want %q", result.GuardrailName, "low_confidence")
			}
		})
	}
}
