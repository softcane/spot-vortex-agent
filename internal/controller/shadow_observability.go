package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type predictDetailedFunc func(context.Context, string, inference.NodeState, float64) (inference.Action, float32, float32, float32, error)
type supportsInstanceTypeFunc func(string) (bool, string)
type runtimeConfigLoaderFunc func() *config.RuntimeConfig

func (c *Controller) runtimeConfigForTick() *config.RuntimeConfig {
	if c != nil && c.runtimeConfigLoader != nil {
		if cfg := c.runtimeConfigLoader(); cfg != nil {
			return cfg
		}
	}
	return c.loadRuntimeConfig()
}

func (c *Controller) supportsInstanceType(instanceType string) (bool, string) {
	if c != nil && c.supportsInstanceTypeOverride != nil {
		return c.supportsInstanceTypeOverride(instanceType)
	}
	if c == nil || c.inf == nil {
		return true, ""
	}
	return c.inf.SupportsInstanceType(instanceType)
}

func (c *Controller) predictDetailed(ctx context.Context, nodeID string, state inference.NodeState, riskMultiplier float64) (inference.Action, float32, float32, float32, error) {
	start := time.Now()
	defer metrics.InferenceLatency.WithLabelValues("pipeline").Observe(time.Since(start).Seconds())

	if c != nil && c.predictDetailedOverride != nil {
		return c.predictDetailedOverride(ctx, nodeID, state, riskMultiplier)
	}
	if c == nil || c.inf == nil {
		return inference.ActionHold, 0, 0, 0, fmt.Errorf("inference engine unavailable")
	}
	return c.inf.PredictDetailed(ctx, nodeID, state, riskMultiplier)
}

func (c *Controller) recordShadowDecisionComparison(
	ctx context.Context,
	poolID string,
	scopeNodeID string,
	state inference.NodeState,
	deterministicAction inference.Action,
	rlAction inference.Action,
	capacityScore float32,
	rlConfidence float32,
	multiplier float64,
) float64 {
	metrics.ShadowActionRecommended.WithLabelValues("rl", inference.ActionToString(rlAction)).Inc()
	if deterministicAction == rlAction {
		metrics.ShadowActionAgreement.WithLabelValues("same").Inc()
	} else {
		metrics.ShadowActionAgreement.WithLabelValues("different").Inc()
	}
	metrics.ShadowActionDelta.WithLabelValues(
		inference.ActionToString(deterministicAction),
		inference.ActionToString(rlAction),
	).Inc()

	if guardrail, blocked := c.shadowGuardrailBlock(ctx, scopeNodeID, rlAction, state, capacityScore, rlConfidence); blocked {
		metrics.ShadowGuardrailBlocked.WithLabelValues(guardrail).Inc()
	}

	if c != nil && c.logger != nil {
		c.logger.Debug("rl shadow comparison",
			"scope", scopeNodeID,
			"pool", poolID,
			"deterministic_action", inference.ActionToString(deterministicAction),
			"rl_shadow_action", inference.ActionToString(rlAction),
		)
	}

	return projectedHourlySavingsDeltaProxyUSD(state, deterministicAction, rlAction, multiplier)
}

func (c *Controller) shadowGuardrailBlock(
	ctx context.Context,
	nodeID string,
	rlAction inference.Action,
	state inference.NodeState,
	capacityScore float32,
	rlConfidence float32,
) (string, bool) {
	if c == nil || c.k8s == nil || nodeID == "" {
		return "", false
	}
	action, ok := toExecutorAction(rlAction)
	if !ok {
		return "", false
	}
	node, err := c.k8s.CoreV1().Nodes().Get(ctx, nodeID, metav1.GetOptions{})
	if err != nil {
		return "", false
	}

	checker := NewGuardrailChecker(c.k8s, c.logger, 0)
	result, err := checker.Check(ctx, node, action, NodeState{
		NodeName:           nodeID,
		InstanceType:       "",
		Zone:               "",
		CapacityScore:      float64(capacityScore),
		SpotPrice:          state.SpotPrice,
		OnDemandPrice:      state.OnDemandPrice,
		PodStartupTime:     state.PodStartupTime,
		OutagePenalty:      state.OutagePenaltyHours,
		Confidence:         float64(rlConfidence),
		ClusterUtilization: state.ClusterUtilization,
	})
	if err != nil || result == nil {
		return "", false
	}
	if !result.Approved {
		return result.GuardrailName, true
	}
	return "", false
}

func projectedHourlySavingsDeltaProxyUSD(
	state inference.NodeState,
	deterministicAction inference.Action,
	rlAction inference.Action,
	multiplier float64,
) float64 {
	if multiplier <= 0 {
		return 0
	}
	spread := state.OnDemandPrice - state.SpotPrice
	if spread == 0 {
		return 0
	}
	rlDelta := actionSpotRatioStepDelta(rlAction, state)
	deterministicDelta := actionSpotRatioStepDelta(deterministicAction, state)
	return spread * (rlDelta - deterministicDelta) * multiplier
}

func actionSpotRatioStepDelta(action inference.Action, state inference.NodeState) float64 {
	switch action {
	case inference.ActionDecrease10:
		return -0.10
	case inference.ActionDecrease30:
		return -0.30
	case inference.ActionIncrease10:
		return 0.10
	case inference.ActionIncrease30:
		return 0.30
	case inference.ActionEmergencyExit:
		return -clampFloat(state.CurrentSpotRatio, 0, 1)
	default:
		return 0
	}
}

func toExecutorAction(action inference.Action) (Action, bool) {
	switch action {
	case inference.ActionHold:
		return ActionHold, true
	case inference.ActionDecrease10:
		return ActionDecrease10, true
	case inference.ActionDecrease30:
		return ActionDecrease30, true
	case inference.ActionIncrease10:
		return ActionIncrease10, true
	case inference.ActionIncrease30:
		return ActionIncrease30, true
	case inference.ActionEmergencyExit:
		return ActionEmergencyExit, true
	default:
		return ActionHold, false
	}
}

func fromExecutorAction(action Action) (inference.Action, bool) {
	switch action {
	case ActionHold:
		return inference.ActionHold, true
	case ActionDecrease10:
		return inference.ActionDecrease10, true
	case ActionDecrease30:
		return inference.ActionDecrease30, true
	case ActionIncrease10:
		return inference.ActionIncrease10, true
	case ActionIncrease30:
		return inference.ActionIncrease30, true
	case ActionEmergencyExit:
		return inference.ActionEmergencyExit, true
	default:
		return inference.ActionHold, false
	}
}
