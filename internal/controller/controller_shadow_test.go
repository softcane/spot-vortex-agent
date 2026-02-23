package controller

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	svmetrics "github.com/softcane/spot-vortex-agent/internal/metrics"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestRunInference_DeterministicMode_RecordsRLShadowAndKeepsDeterministicAction(t *testing.T) {
	k8sClient := k8sfake.NewSimpleClientset()
	createNode(k8sClient, "node-1", "spot", "us-east-1a", "m5.large")

	ctrl, err := New(Config{
		Cloud:               &MockCloudProvider{DryRun: true},
		PriceProvider:       fixedPriceProvider(),
		K8sClient:           k8sClient,
		Inference:           &inference.InferenceEngine{},
		PrometheusClient:    &svmetrics.Client{},
		Logger:              slog.Default(),
		RiskThreshold:       0.95, // avoid Prime Directive override in this test
		MaxDrainRatio:       0.2,
		ReconcileInterval:   10 * time.Second,
		ConfidenceThreshold: 0.5,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	ctrl.runtimeConfigLoader = deterministicRuntimeConfigShadowTest
	ctrl.predictDetailedOverride = func(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
		if nodeID != "node-1" {
			t.Fatalf("unexpected nodeID %q", nodeID)
		}
		// RL suggests increasing Spot, but deterministic should choose DECREASE_30 on high risk.
		return inference.ActionIncrease30, 0.70, 0.10, 0.40, nil
	}

	beforeDeterministic := decisionSourceTotal("deterministic")
	beforeRLDecisionSource := decisionSourceTotal("rl")
	beforeReason := counterVecValue(t, svmetrics.DeterministicDecisionReason, "high_risk")
	beforeShadowRecommended := counterVecValue(t, svmetrics.ShadowActionRecommended, "rl", "INCREASE_30")
	beforeShadowAgreement := counterVecValue(t, svmetrics.ShadowActionAgreement, "different")
	beforeShadowDelta := counterVecValue(t, svmetrics.ShadowActionDelta, "DECREASE_30", "INCREASE_30")
	beforeShadowGuardrail := counterVecValue(t, svmetrics.ShadowGuardrailBlocked, "cluster_fraction")

	assessments, err := ctrl.runInference(context.Background(), []svmetrics.NodeMetrics{
		{
			NodeID:             "node-1",
			InstanceType:       "m5.large",
			Zone:               "us-east-1a",
			IsSpot:             true,
			CPUUsagePercent:    35,
			MemoryUsagePercent: 50,
		},
	})
	if err != nil {
		t.Fatalf("runInference failed: %v", err)
	}
	if len(assessments) != 1 {
		t.Fatalf("expected 1 assessment, got %d", len(assessments))
	}
	got := assessments[0]
	if got.Action != inference.ActionDecrease30 {
		t.Fatalf("active action = %s, want %s", inference.ActionToString(got.Action), inference.ActionToString(inference.ActionDecrease30))
	}
	if !got.HasShadow {
		t.Fatal("expected RL shadow action to be recorded")
	}
	if got.ShadowAction != inference.ActionIncrease30 {
		t.Fatalf("shadow action = %s, want %s", inference.ActionToString(got.ShadowAction), inference.ActionToString(inference.ActionIncrease30))
	}
	if got.Confidence != 1.0 {
		t.Fatalf("deterministic confidence = %v, want 1.0", got.Confidence)
	}
	if got.ClusterUtilization < 0.34 || got.ClusterUtilization > 0.36 {
		t.Fatalf("cluster utilization = %v, want ~0.35 carried from same tick", got.ClusterUtilization)
	}

	if delta := decisionSourceTotal("deterministic") - beforeDeterministic; delta != 1 {
		t.Fatalf("deterministic decision_source_total delta=%v, want 1", delta)
	}
	if delta := decisionSourceTotal("rl") - beforeRLDecisionSource; delta != 0 {
		t.Fatalf("rl decision_source_total delta=%v, want 0 in deterministic-active mode", delta)
	}
	if delta := counterVecValue(t, svmetrics.DeterministicDecisionReason, "high_risk") - beforeReason; delta != 1 {
		t.Fatalf("deterministic reason metric delta=%v, want 1", delta)
	}
	if delta := counterVecValue(t, svmetrics.ShadowActionRecommended, "rl", "INCREASE_30") - beforeShadowRecommended; delta != 1 {
		t.Fatalf("shadow recommended metric delta=%v, want 1", delta)
	}
	if delta := counterVecValue(t, svmetrics.ShadowActionAgreement, "different") - beforeShadowAgreement; delta != 1 {
		t.Fatalf("shadow agreement metric delta=%v, want 1", delta)
	}
	if delta := counterVecValue(t, svmetrics.ShadowActionDelta, "DECREASE_30", "INCREASE_30") - beforeShadowDelta; delta != 1 {
		t.Fatalf("shadow delta metric delta=%v, want 1", delta)
	}
	if delta := counterVecValue(t, svmetrics.ShadowGuardrailBlocked, "cluster_fraction") - beforeShadowGuardrail; delta != 1 {
		t.Fatalf("shadow guardrail blocked metric delta=%v, want 1", delta)
	}

	projectedGauge, err := svmetrics.ShadowProjectedSavingsDeltaUSD.GetMetricWithLabelValues("m5.large:us-east-1a")
	if err != nil {
		t.Fatalf("shadow projected savings gauge lookup failed: %v", err)
	}
	projectedValue := testutil.ToFloat64(projectedGauge)
	if projectedValue <= 0 {
		t.Fatalf("expected positive shadow projected savings delta proxy, got %v", projectedValue)
	}
}

func TestReconcile_DeterministicMode_ActuatesDeterministicActionNotRLShadow(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	k8sClient := k8sfake.NewSimpleClientset()
	createNode(k8sClient, "node-1", "spot", "us-east-1a", "m5.large")

	ctrl, err := New(Config{
		Cloud:                   &MockCloudProvider{DryRun: true},
		PriceProvider:           fixedPriceProvider(),
		K8sClient:               k8sClient,
		Inference:               &inference.InferenceEngine{},
		PrometheusClient:        &svmetrics.Client{},
		Logger:                  slog.Default(),
		RiskThreshold:           0.95, // do not override deterministic policy output
		MaxDrainRatio:           1.0,  // allow single-node actuation in this test; guardrail path is covered separately
		ReconcileInterval:       10 * time.Second,
		ConfidenceThreshold:     0.5,
		DrainGracePeriodSeconds: 1,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	ctrl.runtimeConfigLoader = deterministicRuntimeConfigShadowTest
	ctrl.predictDetailedOverride = func(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
		return inference.ActionIncrease30, 0.70, 0.10, 0.90, nil
	}

	poolID := "m5.large:us-east-1a"
	beforeActionTaken := counterVecValue(t, svmetrics.ActionTaken, "DECREASE_30")
	beforeShadowRecommended := counterVecValue(t, svmetrics.ShadowActionRecommended, "rl", "INCREASE_30")

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if delta := counterVecValue(t, svmetrics.ActionTaken, "DECREASE_30") - beforeActionTaken; delta != 1 {
		t.Fatalf("action_taken_total{action=DECREASE_30} delta=%v, want 1", delta)
	}
	if delta := counterVecValue(t, svmetrics.ShadowActionRecommended, "rl", "INCREASE_30") - beforeShadowRecommended; delta != 1 {
		t.Fatalf("shadow recommendation metric delta=%v, want 1", delta)
	}

	target := ctrl.targetSpotRatio[poolID]
	if target >= 1.0 {
		t.Fatalf("target spot ratio = %v; expected deterministic decrease action to reduce target (and not RL increase)", target)
	}
	if target > 0.71 || target < 0.69 {
		t.Fatalf("target spot ratio = %v, want ~0.70 from deterministic DECREASE_30", target)
	}
}

func TestReconcile_DeterministicMode_RLInferenceFailureDoesNotBreakDeterministicActuation(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	k8sClient := k8sfake.NewSimpleClientset()
	createNode(k8sClient, "node-1", "spot", "us-east-1a", "m5.large")
	createNode(k8sClient, "node-2", "spot", "us-east-1a", "m5.large")

	ctrl, err := New(Config{
		Cloud:                   &MockCloudProvider{DryRun: true},
		PriceProvider:           fixedPriceProvider(),
		K8sClient:               k8sClient,
		Inference:               &inference.InferenceEngine{},
		PrometheusClient:        &svmetrics.Client{},
		Logger:                  slog.Default(),
		RiskThreshold:           0.95,
		MaxDrainRatio:           0.5,
		ReconcileInterval:       10 * time.Second,
		ConfidenceThreshold:     0.5,
		DrainGracePeriodSeconds: 1,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	ctrl.runtimeConfigLoader = deterministicRuntimeConfigShadowTest
	ctrl.predictDetailedOverride = func(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
		if nodeID == "node-1" {
			return inference.ActionHold, 0, 0, 0, fmt.Errorf("RL inference failed: synthetic test error")
		}
		return inference.ActionIncrease10, 0.70, 0.10, 0.90, nil
	}

	beforeActionTaken := counterVecValue(t, svmetrics.ActionTaken, "DECREASE_30")
	beforeDeterministic := decisionSourceTotal("deterministic")

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile should continue when one node inference fails, got error: %v", err)
	}

	if delta := counterVecValue(t, svmetrics.ActionTaken, "DECREASE_30") - beforeActionTaken; delta < 1 {
		t.Fatalf("expected deterministic action to still be actuated for healthy node, action_taken delta=%v", delta)
	}
	if delta := decisionSourceTotal("deterministic") - beforeDeterministic; delta < 1 {
		t.Fatalf("expected deterministic decision source metric to increment for healthy node, delta=%v", delta)
	}
}

func TestReconcile_DeterministicMode_SameNodeRLFallbackStillActuatesDeterministicAction(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	k8sClient := k8sfake.NewSimpleClientset()
	createNode(k8sClient, "node-1", "spot", "us-east-1a", "m5.large")

	ctrl, err := New(Config{
		Cloud:                   &MockCloudProvider{DryRun: true},
		PriceProvider:           fixedPriceProvider(),
		K8sClient:               k8sClient,
		Inference:               &inference.InferenceEngine{},
		PrometheusClient:        &svmetrics.Client{},
		Logger:                  slog.Default(),
		RiskThreshold:           0.95,
		MaxDrainRatio:           1.0,
		ReconcileInterval:       10 * time.Second,
		ConfidenceThreshold:     0.5,
		DrainGracePeriodSeconds: 1,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	ctrl.runtimeConfigLoader = deterministicRuntimeConfigShadowTest
	ctrl.predictDetailedOverride = func(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
		// Simulate RL-only failure after TFT scores are available.
		return inference.ActionHold, 0.70, 0.10, 0.0, &inference.RLFallbackError{
			CapacityScore: 0.70,
			RuntimeScore:  0.10,
			Cause:         fmt.Errorf("RL inference failed: synthetic same-node error"),
		}
	}

	beforeActionTaken := counterVecValue(t, svmetrics.ActionTaken, "DECREASE_30")
	beforeDeterministic := decisionSourceTotal("deterministic")
	beforeShadowRecommended := counterVecValue(t, svmetrics.ShadowActionRecommended, "rl", "HOLD")

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if delta := counterVecValue(t, svmetrics.ActionTaken, "DECREASE_30") - beforeActionTaken; delta != 1 {
		t.Fatalf("expected deterministic fallback action to be actuated, action_taken delta=%v want 1", delta)
	}
	if delta := decisionSourceTotal("deterministic") - beforeDeterministic; delta != 1 {
		t.Fatalf("expected deterministic decision source metric increment, delta=%v want 1", delta)
	}
	if delta := counterVecValue(t, svmetrics.ShadowActionRecommended, "rl", "HOLD") - beforeShadowRecommended; delta != 0 {
		t.Fatalf("expected no RL shadow metric increment on RL fallback failure, delta=%v", delta)
	}
}

func deterministicRuntimeConfigShadowTest() *config.RuntimeConfig {
	cfg := config.DefaultRuntimeConfig()
	cfg.PolicyMode = config.PolicyModeDeterministic
	cfg.RiskMultiplier = 1.0
	cfg.StepMinutes = 30
	return cfg
}

func fixedPriceProvider() *MockPriceProvider {
	return &MockPriceProvider{
		PriceData: cloudapi.SpotPriceData{
			CurrentPrice:  0.2,
			OnDemandPrice: 1.0,
			PriceHistory:  []float64{0.2, 0.21, 0.19},
		},
	}
}

func counterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v) failed: %v", labels, err)
	}
	dtoMetric := &dto.Metric{}
	if err := m.Write(dtoMetric); err != nil {
		t.Fatalf("metric write failed: %v", err)
	}
	if dtoMetric.Counter != nil {
		return dtoMetric.GetCounter().GetValue()
	}
	t.Fatalf("unsupported metric type for labels %v", labels)
	return 0
}
