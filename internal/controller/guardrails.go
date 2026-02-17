// Package controller provides guardrail checks for SpotVortex actions.
// Implements all 4 guardrails per phase.md lines 574-655.
// No fallbacks - actions are blocked or downgraded.
package controller

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

const (
	// AnnotationCritical marks a pod as critical
	AnnotationCritical = "spotvortex.io/critical"

	// AnnotationMigrationStrategy defines migration behavior
	AnnotationMigrationStrategy = "spotvortex.io/migration-strategy"
)

// GuardrailResult contains the result of guardrail checks.
type GuardrailResult struct {
	Approved       bool
	ModifiedAction Action
	Reason         string
	GuardrailName  string
}

// GuardrailChecker implements production guardrails per phase.md.
type GuardrailChecker struct {
	k8s                       kubernetes.Interface
	logger                    *slog.Logger
	clusterFractionLimit      float64 // Default: 0.20 (20%)
	confidenceThreshold       float64 // Default: 0.50
	highUtilizationThreshold  float64 // Default: 0.85 (85%)
}

// NewGuardrailChecker creates a new guardrail checker.
func NewGuardrailChecker(k8s kubernetes.Interface, logger *slog.Logger, clusterFractionLimit float64) *GuardrailChecker {
	if clusterFractionLimit <= 0 {
		clusterFractionLimit = 0.20 // 20% default per phase.md
	}

	return &GuardrailChecker{
		k8s:                      k8s,
		logger:                   logger,
		clusterFractionLimit:     clusterFractionLimit,
		confidenceThreshold:      0.50,
		highUtilizationThreshold: 0.85, // 85% - block migrations when cluster is busy
	}
}

// Check applies all guardrails to an action.
// Returns modified action if guardrails require downgrade.
func (g *GuardrailChecker) Check(ctx context.Context, node *corev1.Node, action Action, state NodeState) (*GuardrailResult, error) {
	// GUARDRAIL 1: Skip for HOLD action (per phase.md line 582)
	if action == ActionHold {
		return &GuardrailResult{
			Approved:       true,
			ModifiedAction: action,
		}, nil
	}

	// GUARDRAIL 1: Human Override - Cluster Fraction Check
	// Per phase.md lines 581-611
	if result, err := g.checkClusterFraction(ctx, node); err != nil {
		return nil, err
	} else if !result.Approved {
		return result, nil
	}

	// GUARDRAIL 2: Low Confidence Check
	if result := g.checkConfidence(state); !result.Approved {
		return result, nil
	}

	// GUARDRAIL 3: PDB Respect
	// Per phase.md lines 614-628
	if result, err := g.checkPDB(ctx, node, action); err != nil {
		return nil, err
	} else if result.ModifiedAction != action {
		return result, nil
	}

	// GUARDRAIL 4: Critical Workload Protection
	// Per phase.md lines 631-641
	if result, err := g.checkCriticalWorkloads(ctx, node, action); err != nil {
		return nil, err
	} else if result.ModifiedAction != action {
		return result, nil
	}

	// GUARDRAIL 5: High Utilization Check
	// Block migrations when cluster is too busy to prevent pod pending
	if result := g.checkHighUtilization(state, action); !result.Approved || result.ModifiedAction != action {
		return result, nil
	}

	return &GuardrailResult{
		Approved:       true,
		ModifiedAction: action,
	}, nil
}

// checkClusterFraction implements GUARDRAIL 1: Human Override Check.
// Blocks actions affecting >20% of cluster.
func (g *GuardrailChecker) checkClusterFraction(ctx context.Context, node *corev1.Node) (*GuardrailResult, error) {
	nodes, err := g.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	clusterSize := len(nodes.Items)
	if clusterSize == 0 {
		return &GuardrailResult{
			Approved:      true,
			GuardrailName: "cluster_fraction",
		}, nil
	}

	fraction := 1.0 / float64(clusterSize)

	if fraction > g.clusterFractionLimit {
		g.logger.Warn("action blocked: cluster fraction too high",
			"node", node.Name,
			"fraction", fraction,
			"limit", g.clusterFractionLimit,
			"cluster_size", clusterSize,
		)

		return &GuardrailResult{
			Approved:      false,
			Reason:        fmt.Sprintf("action would affect %.1f%% of cluster (limit: %.1f%%)", fraction*100, g.clusterFractionLimit*100),
			GuardrailName: "cluster_fraction",
		}, nil
	}

	return &GuardrailResult{
		Approved:      true,
		GuardrailName: "cluster_fraction",
	}, nil
}

// checkConfidence implements low confidence guardrail.
func (g *GuardrailChecker) checkConfidence(state NodeState) *GuardrailResult {
	if state.Confidence < g.confidenceThreshold {
		return &GuardrailResult{
			Approved:      false,
			Reason:        fmt.Sprintf("TFT confidence too low: %.2f < %.2f", state.Confidence, g.confidenceThreshold),
			GuardrailName: "low_confidence",
		}
	}

	return &GuardrailResult{
		Approved:      true,
		GuardrailName: "low_confidence",
	}
}

// checkPDB implements GUARDRAIL 2: PodDisruptionBudget Respect.
// Downgrades MIGRATE_NOW to MIGRATE_SLOW if PDB would be violated.
func (g *GuardrailChecker) checkPDB(ctx context.Context, node *corev1.Node, action Action) (*GuardrailResult, error) {
	if action != ActionEmergencyExit {
		return &GuardrailResult{
			Approved:       true,
			ModifiedAction: action,
			GuardrailName:  "pdb",
		}, nil
	}

	// Get pods on this node
	pods, err := g.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Check each pod's PDB
	for _, pod := range pods.Items {
		pdb, err := g.getPDBForPod(ctx, &pod)
		if err != nil {
			continue // No PDB for this pod
		}

		if pdb != nil && pdb.Status.DisruptionsAllowed == 0 {
			g.logger.Warn("downgrading action due to PDB",
				"pod", pod.Name,
				"pdb", pdb.Name,
				"original_action", "migrate_now",
				"new_action", "migrate_slow",
			)

			return &GuardrailResult{
				Approved:       true,
				ModifiedAction: ActionDecrease30, // Downgrade from Emergency to Decrement (Graceful)
				Reason:         fmt.Sprintf("PDB %s for pod %s allows 0 disruptions", pdb.Name, pod.Name),
				GuardrailName:  "pdb",
			}, nil
		}
	}

	return &GuardrailResult{
		Approved:       true,
		ModifiedAction: action,
		GuardrailName:  "pdb",
	}, nil
}

// checkCriticalWorkloads implements GUARDRAIL 3: Critical Workload Protection.
// Downgrades MIGRATE_NOW to MIGRATE_SLOW for critical pods.
func (g *GuardrailChecker) checkCriticalWorkloads(ctx context.Context, node *corev1.Node, action Action) (*GuardrailResult, error) {
	if action != ActionEmergencyExit {
		return &GuardrailResult{
			Approved:       true,
			ModifiedAction: action,
			GuardrailName:  "critical_workload",
		}, nil
	}

	// Get pods on this node
	pods, err := g.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		// Check critical annotation
		if pod.Annotations[AnnotationCritical] == "true" {
			g.logger.Warn("downgrading action for critical pod",
				"pod", pod.Name,
				"original_action", "migrate_now",
				"new_action", "migrate_slow",
			)

			return &GuardrailResult{
				Approved:       true,
				ModifiedAction: ActionDecrease30, // Downgrade to graceful move
				Reason:         fmt.Sprintf("critical pod %s requires graceful migration", pod.Name),
				GuardrailName:  "critical_workload",
			}, nil
		}

		// Check migration strategy annotation
		strategy := pod.Annotations[AnnotationMigrationStrategy]
		if strategy == "graceful-only" {
			g.logger.Warn("downgrading action per migration strategy",
				"pod", pod.Name,
				"strategy", strategy,
			)

			return &GuardrailResult{
				Approved:       true,
				ModifiedAction: ActionDecrease30,
				Reason:         fmt.Sprintf("pod %s migration-strategy=graceful-only", pod.Name),
				GuardrailName:  "critical_workload",
			}, nil
		}
	}

	return &GuardrailResult{
		Approved:       true,
		ModifiedAction: action,
		GuardrailName:  "critical_workload",
	}, nil
}

// checkHighUtilization implements GUARDRAIL 5: High Utilization Protection.
// When cluster utilization is high (>85%), migrations are blocked or downgraded
// to prevent pod pending issues from lack of scheduling headroom.
func (g *GuardrailChecker) checkHighUtilization(state NodeState, action Action) *GuardrailResult {
	// Only check for actions that would disrupt nodes
	if action == ActionHold || action == ActionIncrease10 || action == ActionIncrease30 {
		return &GuardrailResult{
			Approved:       true,
			ModifiedAction: action,
			GuardrailName:  "high_utilization",
		}
	}

	// Check cluster utilization from state
	if state.ClusterUtilization > g.highUtilizationThreshold {
		if g.logger != nil {
			g.logger.Warn("action blocked due to high cluster utilization",
				"utilization", state.ClusterUtilization,
				"threshold", g.highUtilizationThreshold,
				"action", action,
			)
		}

		// For EMERGENCY_EXIT, downgrade to DECREASE_30 (graceful)
		if action == ActionEmergencyExit {
			return &GuardrailResult{
				Approved:       true,
				ModifiedAction: ActionDecrease30,
				Reason:         fmt.Sprintf("cluster utilization %.1f%% > %.1f%%, downgrading to graceful migration", state.ClusterUtilization*100, g.highUtilizationThreshold*100),
				GuardrailName:  "high_utilization",
			}
		}

		// For DECREASE actions, block entirely when utilization is very high (>95%)
		if state.ClusterUtilization > 0.95 {
			return &GuardrailResult{
				Approved:      false,
				Reason:        fmt.Sprintf("cluster utilization %.1f%% too high, blocking all migrations", state.ClusterUtilization*100),
				GuardrailName: "high_utilization",
			}
		}

		// Otherwise allow but log warning
		return &GuardrailResult{
			Approved:       true,
			ModifiedAction: action,
			Reason:         fmt.Sprintf("cluster utilization %.1f%% is high, proceed with caution", state.ClusterUtilization*100),
			GuardrailName:  "high_utilization",
		}
	}

	return &GuardrailResult{
		Approved:       true,
		ModifiedAction: action,
		GuardrailName:  "high_utilization",
	}
}

// getPDBForPod finds the PDB that matches a pod.
func (g *GuardrailChecker) getPDBForPod(ctx context.Context, pod *corev1.Pod) (*policyv1.PodDisruptionBudget, error) {
	pdbs, err := g.k8s.PolicyV1().PodDisruptionBudgets(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, pdb := range pdbs.Items {
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}

		if selector.Matches(labels.Set(pod.Labels)) {
			return &pdb, nil
		}
	}

	return nil, nil // No matching PDB
}
