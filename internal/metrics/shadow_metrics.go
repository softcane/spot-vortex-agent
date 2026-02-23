package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ShadowActionRecommended counts RL shadow recommendations in deterministic-active mode.
	// source is kept explicit to make future extensions easy without changing metric shape.
	ShadowActionRecommended = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "shadow_action_recommended_total",
			Help:      "Total RL shadow recommendations grouped by source and action",
		},
		[]string{"source", "action"},
	)

	// ShadowActionAgreement counts whether RL shadow matched the deterministic active action.
	ShadowActionAgreement = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "shadow_action_agreement_total",
			Help:      "Total comparisons of deterministic vs RL shadow actions grouped by agreement outcome",
		},
		[]string{"agreement"},
	)

	// ShadowActionDelta counts deterministic-vs-RL action pairs for mismatch analysis.
	ShadowActionDelta = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "shadow_action_delta_total",
			Help:      "Total comparisons grouped by deterministic action and RL shadow action",
		},
		[]string{"deterministic_action", "rl_action"},
	)

	// ShadowProjectedSavingsDeltaUSD is an instantaneous per-pool proxy.
	// Positive means RL shadow suggests more spot savings than deterministic for the current tick.
	ShadowProjectedSavingsDeltaUSD = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "spotvortex",
			Name:      "shadow_projected_savings_delta_usd",
			Help:      "Per-pool projected hourly savings delta proxy (RL shadow minus deterministic) based on current spread and action step",
		},
		[]string{"pool"},
	)

	// ShadowGuardrailBlocked counts RL shadow suggestions that would be blocked by guardrails.
	ShadowGuardrailBlocked = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "spotvortex",
			Name:      "shadow_guardrail_blocked_total",
			Help:      "RL shadow suggestions that would be blocked by guardrails, grouped by guardrail name",
		},
		[]string{"guardrail"},
	)
)

func init() {
	for _, action := range []string{
		"HOLD",
		"DECREASE_10",
		"DECREASE_30",
		"INCREASE_10",
		"INCREASE_30",
		"EMERGENCY_EXIT",
	} {
		ShadowActionRecommended.WithLabelValues("rl", action).Add(0)
	}
	for _, agreement := range []string{"same", "different"} {
		ShadowActionAgreement.WithLabelValues(agreement).Add(0)
	}
	ShadowActionDelta.WithLabelValues("HOLD", "HOLD").Add(0)
	for _, guardrail := range []string{
		"cluster_fraction",
		"low_confidence",
		"pdb",
		"critical_workload",
		"high_utilization",
	} {
		ShadowGuardrailBlocked.WithLabelValues(guardrail).Add(0)
	}
}
