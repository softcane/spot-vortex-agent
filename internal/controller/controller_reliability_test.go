package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	svmetrics "github.com/softcane/spot-vortex-agent/internal/metrics"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

type staticReliabilityCollector struct {
	snapshot svmetrics.ReliabilityTelemetrySnapshot
	err      error
	calls    int
}

func (c *staticReliabilityCollector) CollectReliabilityTelemetry(context.Context) (svmetrics.ReliabilityTelemetrySnapshot, error) {
	c.calls++
	return c.snapshot, c.err
}

func TestReconcile_RecordsReliabilityTelemetryFromCollector(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	k8sClient := k8sfake.NewSimpleClientset()
	createNode(k8sClient, "node-1", "spot", "us-east-1a", "m5.large")

	collector := &staticReliabilityCollector{
		snapshot: svmetrics.ReliabilityTelemetrySnapshot{
			AWSInterruptionNotices:      1,
			AWSRebalanceRecommendations: 2,
			NodeTerminations:            3,
			NodeNotReadyTransitions:     4,
			PodEvictions:                5,
			PodRestarts:                 6,
			PodPendingDurationsSeconds:  []float64{1.5, 2.5},
			RecoveryDurationsSeconds:    []float64{3.5},
		},
	}

	ctrl, err := New(Config{
		Cloud:                         &MockCloudProvider{DryRun: true},
		PriceProvider:                 fixedPriceProvider(),
		K8sClient:                     k8sClient,
		Inference:                     &inference.InferenceEngine{},
		PrometheusClient:              &svmetrics.Client{},
		Logger:                        slog.Default(),
		RiskThreshold:                 0.95,
		MaxDrainRatio:                 1.0,
		ReconcileInterval:             10 * time.Second,
		ConfidenceThreshold:           0.5,
		DrainGracePeriodSeconds:       1,
		ReliabilityTelemetryCollector: collector,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	ctrl.runtimeConfigLoader = func() *config.RuntimeConfig { return config.DefaultRuntimeConfig() }
	ctrl.predictDetailedOverride = func(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
		return inference.ActionHold, 0.1, 0.1, 1.0, nil
	}

	beforeInterruptions := testutil.ToFloat64(svmetrics.AWSInterruptionNoticeTotal)
	beforeRebalance := testutil.ToFloat64(svmetrics.AWSRebalanceRecommendationTotal)
	beforeTerminations := testutil.ToFloat64(svmetrics.NodeTerminationTotal)
	beforeNotReady := testutil.ToFloat64(svmetrics.NodeNotReadyTotal)
	beforeEvictions := testutil.ToFloat64(svmetrics.PodEvictionsTotal)
	beforeRestarts := testutil.ToFloat64(svmetrics.PodRestartsTotal)
	beforePendingSamples := histogramSampleCountController(t, svmetrics.PodPendingDurationSeconds)
	beforeRecoverySamples := histogramSampleCountController(t, svmetrics.RecoveryTimeSeconds)

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if collector.calls != 1 {
		t.Fatalf("collector calls=%d, want 1", collector.calls)
	}
	if delta := testutil.ToFloat64(svmetrics.AWSInterruptionNoticeTotal) - beforeInterruptions; delta != 1 {
		t.Fatalf("interruptions delta=%v, want 1", delta)
	}
	if delta := testutil.ToFloat64(svmetrics.AWSRebalanceRecommendationTotal) - beforeRebalance; delta != 2 {
		t.Fatalf("rebalance delta=%v, want 2", delta)
	}
	if delta := testutil.ToFloat64(svmetrics.NodeTerminationTotal) - beforeTerminations; delta != 3 {
		t.Fatalf("terminations delta=%v, want 3", delta)
	}
	if delta := testutil.ToFloat64(svmetrics.NodeNotReadyTotal) - beforeNotReady; delta != 4 {
		t.Fatalf("notready delta=%v, want 4", delta)
	}
	if delta := testutil.ToFloat64(svmetrics.PodEvictionsTotal) - beforeEvictions; delta != 5 {
		t.Fatalf("evictions delta=%v, want 5", delta)
	}
	if delta := testutil.ToFloat64(svmetrics.PodRestartsTotal) - beforeRestarts; delta != 6 {
		t.Fatalf("restarts delta=%v, want 6", delta)
	}
	if delta := histogramSampleCountController(t, svmetrics.PodPendingDurationSeconds) - beforePendingSamples; delta != 2 {
		t.Fatalf("pending histogram sample delta=%v, want 2", delta)
	}
	if delta := histogramSampleCountController(t, svmetrics.RecoveryTimeSeconds) - beforeRecoverySamples; delta != 1 {
		t.Fatalf("recovery histogram sample delta=%v, want 1", delta)
	}
}

func TestReconcile_DefaultNoopReliabilityCollector_DoesNotEmitFakeTelemetry(t *testing.T) {
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
	ctrl.runtimeConfigLoader = func() *config.RuntimeConfig { return config.DefaultRuntimeConfig() }
	ctrl.predictDetailedOverride = func(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
		return inference.ActionHold, 0.1, 0.1, 1.0, nil
	}

	beforeInterruptions := testutil.ToFloat64(svmetrics.AWSInterruptionNoticeTotal)
	beforePendingSamples := histogramSampleCountController(t, svmetrics.PodPendingDurationSeconds)

	if err := ctrl.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile failed with default noop collector: %v", err)
	}

	if delta := testutil.ToFloat64(svmetrics.AWSInterruptionNoticeTotal) - beforeInterruptions; delta != 0 {
		t.Fatalf("expected noop collector to keep interruption metric unchanged, delta=%v", delta)
	}
	if delta := histogramSampleCountController(t, svmetrics.PodPendingDurationSeconds) - beforePendingSamples; delta != 0 {
		t.Fatalf("expected noop collector to keep pending histogram unchanged, delta=%v", delta)
	}
}

func histogramSampleCountController(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := h.Write(m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}
