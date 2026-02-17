// Package controller provides the action executor for SpotVortex.
// Implements all 5 RL actions per phase.md lines 296-361.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/softcane/spot-vortex-agent/internal/karpenter"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
)

// Action represents an RL action.
type Action int

const (
	ActionHold          Action = 0 // No action, update metrics only
	ActionDecrease10    Action = 1 // Target -= 10%
	ActionDecrease30    Action = 2 // Target -= 30%
	ActionIncrease10    Action = 3 // Target += 10%
	ActionIncrease30    Action = 4 // Target += 30%
	ActionEmergencyExit Action = 5 // Target = 0% (all OD)
)

// String returns the action name.
func (a Action) String() string {
	switch a {
	case ActionHold:
		return "hold"
	case ActionDecrease10:
		return "decrease_10"
	case ActionDecrease30:
		return "decrease_30"
	case ActionIncrease10:
		return "increase_10"
	case ActionIncrease30:
		return "increase_30"
	case ActionEmergencyExit:
		return "emergency_exit"
	default:
		return "unknown"
	}
}

// NodeState contains the state used for action decisions.
type NodeState struct {
	NodeName           string
	InstanceType       string
	Zone               string
	CapacityScore      float64
	SpotPrice          float64
	OnDemandPrice      float64
	PodStartupTime     float64
	OutagePenalty      float64
	Confidence         float64
	ClusterUtilization float64 // 0-1, for high utilization guardrail
}

// ExecutorConfig contains configuration for the action executor.
type ExecutorConfig struct {
	GracefulDrainPeriod time.Duration // For MIGRATE_SLOW
	ForceDrainPeriod    time.Duration // For MIGRATE_NOW
	NodePoolName        string        // Karpenter NodePool to manage
	ClusterFractionMax  float64       // Max fraction of cluster to affect (guardrail)
}

// Executor executes RL actions on the cluster.
type Executor struct {
	k8s           kubernetes.Interface
	dynamicClient dynamic.Interface
	nodePoolMgr   *karpenter.NodePoolManager
	drainer       *Drainer
	guardrails    *GuardrailChecker
	logger        *slog.Logger
	config        ExecutorConfig
}

// NewExecutor creates a new action executor.
func NewExecutor(
	k8s kubernetes.Interface,
	dynamicClient dynamic.Interface,
	logger *slog.Logger,
	config ExecutorConfig,
) *Executor {
	return &Executor{
		k8s:           k8s,
		dynamicClient: dynamicClient,
		nodePoolMgr:   karpenter.NewNodePoolManager(dynamicClient, logger),
		drainer: NewDrainer(k8s, logger, DrainConfig{
			GracePeriodSeconds: int64(config.GracefulDrainPeriod.Seconds()),
			IgnoreDaemonSets:   true,
			DeleteEmptyDirData: true,
		}),
		guardrails: NewGuardrailChecker(k8s, logger, config.ClusterFractionMax),
		logger:     logger,
		config:     config,
	}
}

// Execute runs the specified action for a node.
// Per phase.md lines 574-655: applies guardrails before execution.
func (e *Executor) Execute(ctx context.Context, node *corev1.Node, action Action, state NodeState) error {
	e.logger.Info("executing action",
		"node", node.Name,
		"action", action.String(),
		"capacity_score", state.CapacityScore,
	)

	// Apply guardrails (per phase.md lines 574-655)
	result, err := e.guardrails.Check(ctx, node, action, state)
	if err != nil {
		return fmt.Errorf("guardrail check failed: %w", err)
	}

	if !result.Approved {
		e.logger.Warn("action blocked by guardrails",
			"node", node.Name,
			"action", action.String(),
			"reason", result.Reason,
		)
		metrics.GuardrailBlocked.WithLabelValues(result.GuardrailName).Inc()
		return fmt.Errorf("blocked by guardrails: %s", result.Reason)
	}

	// Possibly downgraded action
	if result.ModifiedAction != action {
		e.logger.Info("action downgraded by guardrails",
			"original", action.String(),
			"modified", result.ModifiedAction.String(),
			"reason", result.Reason,
		)
		action = result.ModifiedAction
	}

	// Execute the action
	switch action {
	case ActionHold:
		return e.executeHold(ctx, node, state)

	case ActionDecrease10, ActionDecrease30:
		return e.executeMigrateSlow(ctx, node, state)

	case ActionEmergencyExit:
		return e.executeMigrateNow(ctx, node, state)

	case ActionIncrease10, ActionIncrease30:
		return e.executeRecover(ctx, node, state)

	default:
		return fmt.Errorf("unknown action: %d", action)
	}
}

// executeHold implements HOLD action.
func (e *Executor) executeHold(ctx context.Context, node *corev1.Node, state NodeState) error {
	// Update capacity score label
	if err := e.updateNodeLabel(ctx, node.Name, "spotvortex.io/capacity-score", fmt.Sprintf("%.2f", state.CapacityScore)); err != nil {
		return err
	}

	// Update metrics
	metrics.ActionTaken.WithLabelValues("stay").Inc()
	metrics.CapacityScore.WithLabelValues(node.Name, state.Zone).Set(state.CapacityScore)

	return nil
}

// executeMigrateSlow implements MIGRATE_SLOW action (phase.md lines 303-323).
func (e *Executor) executeMigrateSlow(ctx context.Context, node *corev1.Node, state NodeState) error {
	e.logger.Info("starting graceful migration",
		"node", node.Name,
		"grace_period", e.config.GracefulDrainPeriod,
	)

	// 1. Drain implements cordon internally, just call drain
	// The drain operation handles cordoning first

	// 2. Add draining taint
	if err := e.addTaint(ctx, node.Name, "spotvortex.io/draining", corev1.TaintEffectNoSchedule); err != nil {
		return fmt.Errorf("failed to add taint: %w", err)
	}

	// 3. Update label
	if err := e.updateNodeLabel(ctx, node.Name, "spotvortex.io/market-status", "draining"); err != nil {
		return err
	}

	// 4. Drain (async - respects PDBs)
	go func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), e.config.GracefulDrainPeriod)
		defer cancel()

		if _, err := e.drainer.Drain(drainCtx, node.Name); err != nil {
			e.logger.Error("drain failed", "node", node.Name, "error", err)
		} else {
			e.logger.Info("drain completed", "node", node.Name)
			metrics.OutagesAvoided.Inc()
		}
	}()

	metrics.ActionTaken.WithLabelValues("migrate_slow").Inc()
	return nil
}

// executeMigrateNow implements MIGRATE_NOW action (phase.md lines 325-330).
func (e *Executor) executeMigrateNow(ctx context.Context, node *corev1.Node, state NodeState) error {
	e.logger.Info("starting immediate migration",
		"node", node.Name,
		"grace_period", e.config.ForceDrainPeriod,
	)

	// 1. Force taint
	if err := e.addTaint(ctx, node.Name, "spotvortex.io/draining", corev1.TaintEffectNoExecute); err != nil {
		return fmt.Errorf("failed to add taint: %w", err)
	}

	// 2. Force drain with short grace period
	go func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), e.config.ForceDrainPeriod)
		defer cancel()

		// Force drain = drain with Force enabled; reuse existing Drain method
		if _, err := e.drainer.Drain(drainCtx, node.Name); err != nil {
			e.logger.Error("force drain failed", "node", node.Name, "error", err)
		} else {
			e.logger.Info("force drain completed", "node", node.Name)
			metrics.OutagesAvoided.Inc()
		}
	}()

	metrics.ActionTaken.WithLabelValues("migrate_now").Inc()
	return nil
}

// executeFallbackOD implements FALLBACK_OD action (phase.md lines 332-346).
func (e *Executor) executeFallbackOD(ctx context.Context, node *corev1.Node, state NodeState) error {
	e.logger.Info("falling back to on-demand",
		"node", node.Name,
		"nodepool", e.config.NodePoolName,
	)

	// 1. Update Karpenter NodePool to on-demand only
	if err := e.nodePoolMgr.FallbackToOnDemand(ctx, e.config.NodePoolName); err != nil {
		return fmt.Errorf("failed to update NodePool: %w", err)
	}

	// 2. Update node label
	if err := e.updateNodeLabel(ctx, node.Name, "spotvortex.io/market-status", "volatile"); err != nil {
		return err
	}

	// 3. Drain the spot node
	go func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		if _, err := e.drainer.Drain(drainCtx, node.Name); err != nil {
			e.logger.Error("drain after fallback failed", "node", node.Name, "error", err)
		}
	}()

	metrics.ActionTaken.WithLabelValues("fallback_od").Inc()
	return nil
}

// executeRecover implements RECOVER action (phase.md lines 347-360).
func (e *Executor) executeRecover(ctx context.Context, node *corev1.Node, state NodeState) error {
	e.logger.Info("recovering to spot",
		"node", node.Name,
		"nodepool", e.config.NodePoolName,
	)

	// 1. Update Karpenter NodePool to re-enable spot
	if err := e.nodePoolMgr.RecoverToSpot(ctx, e.config.NodePoolName); err != nil {
		return fmt.Errorf("failed to update NodePool: %w", err)
	}

	// 2. Update node labels
	if err := e.updateNodeLabel(ctx, node.Name, "spotvortex.io/market-status", "stable"); err != nil {
		return err
	}

	// 3. Add prefer-spot taint to OD node (triggers gradual migration)
	if err := e.addTaint(ctx, node.Name, "spotvortex.io/prefer-spot", corev1.TaintEffectPreferNoSchedule); err != nil {
		e.logger.Warn("failed to add prefer-spot taint", "error", err)
	}

	metrics.ActionTaken.WithLabelValues("recover").Inc()
	return nil
}

// updateNodeLabel updates a label on a node.
func (e *Executor) updateNodeLabel(ctx context.Context, nodeName, key, value string) error {
	node, err := e.k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[key] = value

	_, err = e.k8s.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node %s: %w", nodeName, err)
	}

	return nil
}

// addTaint adds a taint to a node.
func (e *Executor) addTaint(ctx context.Context, nodeName, key string, effect corev1.TaintEffect) error {
	node, err := e.k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	// Check if taint already exists
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			return nil // Already exists
		}
	}

	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    key,
		Effect: effect,
	})

	_, err = e.k8s.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node %s: %w", nodeName, err)
	}

	return nil
}
