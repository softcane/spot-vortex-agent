package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Real-world reliability telemetry counters (zero until a real collector is wired).
	AWSInterruptionNoticeTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "aws_interruption_notice_total",
			Help:      "Total AWS Spot interruption notices observed by the agent",
		},
	)

	AWSRebalanceRecommendationTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "aws_rebalance_recommendation_total",
			Help:      "Total AWS rebalance recommendations observed by the agent",
		},
	)

	NodeTerminationTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "node_termination_total",
			Help:      "Total node termination events observed",
		},
	)

	NodeNotReadyTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "node_notready_total",
			Help:      "Total node NotReady transition events observed",
		},
	)

	PodEvictionsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "pod_evictions_total",
			Help:      "Total pod eviction events observed",
		},
	)

	PodRestartsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "pod_restarts_total",
			Help:      "Total pod restart events observed",
		},
	)

	PodPendingDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "spotvortex",
			Name:      "pod_pending_duration_seconds",
			Help:      "Observed pod pending durations in seconds",
			Buckets:   prometheus.DefBuckets,
		},
	)

	RecoveryTimeSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "spotvortex",
			Name:      "recovery_time_seconds",
			Help:      "Observed recovery times in seconds for workload/service restoration",
			Buckets:   prometheus.DefBuckets,
		},
	)
)

// ReliabilityTelemetrySnapshot is a truthful container for real cluster/provider signals.
// TODO: Wire a real collector from Kubernetes events / provider notifications.
type ReliabilityTelemetrySnapshot struct {
	AWSInterruptionNotices      uint64
	AWSRebalanceRecommendations uint64
	NodeTerminations            uint64
	NodeNotReadyTransitions     uint64
	PodEvictions                uint64
	PodRestarts                 uint64
	PodPendingDurationsSeconds  []float64
	RecoveryDurationsSeconds    []float64
}

// ReliabilityTelemetryCollector is the interface for real reliability signal collection.
type ReliabilityTelemetryCollector interface {
	CollectReliabilityTelemetry(ctx context.Context) (ReliabilityTelemetrySnapshot, error)
}

// NoopReliabilityTelemetryCollector emits no fake data. It exists so metrics can be wired safely.
type NoopReliabilityTelemetryCollector struct{}

func (NoopReliabilityTelemetryCollector) CollectReliabilityTelemetry(context.Context) (ReliabilityTelemetrySnapshot, error) {
	return ReliabilityTelemetrySnapshot{}, nil
}

// RecordReliabilityTelemetry records observed deltas/samples into Prometheus metrics.
func RecordReliabilityTelemetry(snapshot ReliabilityTelemetrySnapshot) {
	if snapshot.AWSInterruptionNotices > 0 {
		AWSInterruptionNoticeTotal.Add(float64(snapshot.AWSInterruptionNotices))
	}
	if snapshot.AWSRebalanceRecommendations > 0 {
		AWSRebalanceRecommendationTotal.Add(float64(snapshot.AWSRebalanceRecommendations))
	}
	if snapshot.NodeTerminations > 0 {
		NodeTerminationTotal.Add(float64(snapshot.NodeTerminations))
	}
	if snapshot.NodeNotReadyTransitions > 0 {
		NodeNotReadyTotal.Add(float64(snapshot.NodeNotReadyTransitions))
	}
	if snapshot.PodEvictions > 0 {
		PodEvictionsTotal.Add(float64(snapshot.PodEvictions))
	}
	if snapshot.PodRestarts > 0 {
		PodRestartsTotal.Add(float64(snapshot.PodRestarts))
	}
	for _, v := range snapshot.PodPendingDurationsSeconds {
		if v >= 0 {
			PodPendingDurationSeconds.Observe(v)
		}
	}
	for _, v := range snapshot.RecoveryDurationsSeconds {
		if v >= 0 {
			RecoveryTimeSeconds.Observe(v)
		}
	}
}
