package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestShadowMetrics_RecordAndExpose(t *testing.T) {
	beforeRecommended := counterVecValue(t, ShadowActionRecommended, "rl", "INCREASE_30")
	beforeAgreement := counterVecValue(t, ShadowActionAgreement, "different")
	beforeDelta := counterVecValue(t, ShadowActionDelta, "DECREASE_30", "INCREASE_30")
	beforeBlocked := counterVecValue(t, ShadowGuardrailBlocked, "cluster_fraction")

	ShadowActionRecommended.WithLabelValues("rl", "INCREASE_30").Inc()
	ShadowActionAgreement.WithLabelValues("different").Inc()
	ShadowActionDelta.WithLabelValues("DECREASE_30", "INCREASE_30").Inc()
	ShadowGuardrailBlocked.WithLabelValues("cluster_fraction").Inc()
	ShadowProjectedSavingsDeltaUSD.WithLabelValues("pool-a").Set(1.25)

	if got := counterVecValue(t, ShadowActionRecommended, "rl", "INCREASE_30"); got-beforeRecommended != 1 {
		t.Fatalf("shadow_action_recommended_total delta=%v, want 1", got-beforeRecommended)
	}
	if got := counterVecValue(t, ShadowActionAgreement, "different"); got-beforeAgreement != 1 {
		t.Fatalf("shadow_action_agreement_total delta=%v, want 1", got-beforeAgreement)
	}
	if got := counterVecValue(t, ShadowActionDelta, "DECREASE_30", "INCREASE_30"); got-beforeDelta != 1 {
		t.Fatalf("shadow_action_delta_total delta=%v, want 1", got-beforeDelta)
	}
	if got := counterVecValue(t, ShadowGuardrailBlocked, "cluster_fraction"); got-beforeBlocked != 1 {
		t.Fatalf("shadow_guardrail_blocked_total delta=%v, want 1", got-beforeBlocked)
	}

	gauge, err := ShadowProjectedSavingsDeltaUSD.GetMetricWithLabelValues("pool-a")
	if err != nil {
		t.Fatalf("get shadow projected savings gauge: %v", err)
	}
	if got := testutil.ToFloat64(gauge); got != 1.25 {
		t.Fatalf("shadow_projected_savings_delta_usd=%v, want 1.25", got)
	}

	assertMetricFamiliesPresent(t,
		"spotvortex_shadow_action_recommended_total",
		"spotvortex_shadow_action_agreement_total",
		"spotvortex_shadow_action_delta_total",
		"spotvortex_shadow_projected_savings_delta_usd",
		"spotvortex_shadow_guardrail_blocked_total",
	)
}

func TestReliabilityTelemetry_NoopAndRecord(t *testing.T) {
	collector := NoopReliabilityTelemetryCollector{}
	snapshot, err := collector.CollectReliabilityTelemetry(context.Background())
	if err != nil {
		t.Fatalf("noop collector returned error: %v", err)
	}
	if snapshot.AWSInterruptionNotices != 0 ||
		snapshot.AWSRebalanceRecommendations != 0 ||
		snapshot.NodeTerminations != 0 ||
		snapshot.NodeNotReadyTransitions != 0 ||
		snapshot.PodEvictions != 0 ||
		snapshot.PodRestarts != 0 ||
		len(snapshot.PodPendingDurationsSeconds) != 0 ||
		len(snapshot.RecoveryDurationsSeconds) != 0 {
		t.Fatalf("noop collector should return zero snapshot, got %+v", snapshot)
	}

	beforeInterruptions := testutil.ToFloat64(AWSInterruptionNoticeTotal)
	beforeRebalance := testutil.ToFloat64(AWSRebalanceRecommendationTotal)
	beforeTerminations := testutil.ToFloat64(NodeTerminationTotal)
	beforeNotReady := testutil.ToFloat64(NodeNotReadyTotal)
	beforeEvictions := testutil.ToFloat64(PodEvictionsTotal)
	beforeRestarts := testutil.ToFloat64(PodRestartsTotal)
	beforePendingSamples := histogramSampleCount(t, PodPendingDurationSeconds)
	beforeRecoverySamples := histogramSampleCount(t, RecoveryTimeSeconds)

	RecordReliabilityTelemetry(ReliabilityTelemetrySnapshot{})

	if got := testutil.ToFloat64(AWSInterruptionNoticeTotal); got != beforeInterruptions {
		t.Fatalf("zero snapshot should not change interruption counter: got %v want %v", got, beforeInterruptions)
	}
	if got := histogramSampleCount(t, PodPendingDurationSeconds); got != beforePendingSamples {
		t.Fatalf("zero snapshot should not change pending histogram count: got %v want %v", got, beforePendingSamples)
	}

	RecordReliabilityTelemetry(ReliabilityTelemetrySnapshot{
		AWSInterruptionNotices:      1,
		AWSRebalanceRecommendations: 2,
		NodeTerminations:            3,
		NodeNotReadyTransitions:     4,
		PodEvictions:                5,
		PodRestarts:                 6,
		PodPendingDurationsSeconds:  []float64{1.2, 2.4},
		RecoveryDurationsSeconds:    []float64{3.6},
	})

	if got := testutil.ToFloat64(AWSInterruptionNoticeTotal) - beforeInterruptions; got != 1 {
		t.Fatalf("interruptions delta=%v, want 1", got)
	}
	if got := testutil.ToFloat64(AWSRebalanceRecommendationTotal) - beforeRebalance; got != 2 {
		t.Fatalf("rebalance delta=%v, want 2", got)
	}
	if got := testutil.ToFloat64(NodeTerminationTotal) - beforeTerminations; got != 3 {
		t.Fatalf("termination delta=%v, want 3", got)
	}
	if got := testutil.ToFloat64(NodeNotReadyTotal) - beforeNotReady; got != 4 {
		t.Fatalf("notready delta=%v, want 4", got)
	}
	if got := testutil.ToFloat64(PodEvictionsTotal) - beforeEvictions; got != 5 {
		t.Fatalf("evictions delta=%v, want 5", got)
	}
	if got := testutil.ToFloat64(PodRestartsTotal) - beforeRestarts; got != 6 {
		t.Fatalf("restarts delta=%v, want 6", got)
	}
	if got := histogramSampleCount(t, PodPendingDurationSeconds) - beforePendingSamples; got != 2 {
		t.Fatalf("pending histogram sample delta=%v, want 2", got)
	}
	if got := histogramSampleCount(t, RecoveryTimeSeconds) - beforeRecoverySamples; got != 1 {
		t.Fatalf("recovery histogram sample delta=%v, want 1", got)
	}

	assertMetricFamiliesPresent(t,
		"spotvortex_aws_interruption_notice_total",
		"spotvortex_aws_rebalance_recommendation_total",
		"spotvortex_node_termination_total",
		"spotvortex_node_notready_total",
		"spotvortex_pod_evictions_total",
		"spotvortex_pod_restarts_total",
		"spotvortex_pod_pending_duration_seconds",
		"spotvortex_recovery_time_seconds",
	)
}

func TestMetricsHTTPExposeShadowAndReliabilityMetricNames(t *testing.T) {
	// Ensure labeled shadow metrics have instantiated series before scraping.
	ShadowActionRecommended.WithLabelValues("rl", "HOLD").Add(0)
	ShadowActionAgreement.WithLabelValues("same").Add(0)
	ShadowActionDelta.WithLabelValues("HOLD", "HOLD").Add(0)
	ShadowProjectedSavingsDeltaUSD.WithLabelValues("pool-test").Set(0)
	ShadowGuardrailBlocked.WithLabelValues("cluster_fraction").Add(0)

	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body failed: %v", err)
	}
	text := string(body)
	for _, name := range []string{
		"spotvortex_shadow_action_recommended_total",
		"spotvortex_shadow_action_agreement_total",
		"spotvortex_shadow_action_delta_total",
		"spotvortex_shadow_projected_savings_delta_usd",
		"spotvortex_shadow_guardrail_blocked_total",
		"spotvortex_aws_interruption_notice_total",
		"spotvortex_recovery_time_seconds",
	} {
		if !strings.Contains(text, name) {
			t.Fatalf("metrics HTTP output missing %q", name)
		}
	}
}

func counterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get counter metric with labels %v: %v", labels, err)
	}
	return testutil.ToFloat64(m)
}

func histogramSampleCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := h.Write(m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func assertMetricFamiliesPresent(t *testing.T, names ...string) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	index := make(map[string]struct{}, len(families))
	for _, mf := range families {
		index[mf.GetName()] = struct{}{}
	}
	for _, name := range names {
		if _, ok := index[name]; !ok {
			t.Fatalf("metric family %q not found", name)
		}
	}
}
