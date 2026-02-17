// Package karpenter provides Karpenter-specific integration for SpotVortex.
//
// When Karpenter is detected, SpotVortex operates in "Label-Only Mode":
// - Instead of cordoning/draining nodes directly
// - It applies the spotvortex.io/risk label
// - Karpenter's Drift mechanism handles the rest
//
// Architecture: architecture.md (Karpenter Provider)
// Guardrails: mission_guardrail.md (Karpenter First)
package karpenter

import (
	"context"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// RiskLabel is the label key used to signal risk to Karpenter.
	RiskLabel = "spotvortex.io/risk"

	// RiskLow indicates the node is safe.
	RiskLow = "low"

	// RiskHigh indicates the node should be replaced.
	RiskHigh = "high"

	// MarketStatusLabel indicates overall market status for the zone.
	MarketStatusLabel = "spotvortex.io/market-status"

	// MarketStable indicates the spot market is stable.
	MarketStable = "stable"

	// MarketVolatile indicates the spot market is volatile.
	MarketVolatile = "volatile"
)

// Labeler manages Karpenter-compatible node labels.
type Labeler struct {
	k8s    kubernetes.Interface
	logger *slog.Logger
	dryRun bool
}

// NewLabeler creates a new Karpenter labeler.
func NewLabeler(k8s kubernetes.Interface, logger *slog.Logger, dryRun bool) *Labeler {
	return &Labeler{
		k8s:    k8s,
		logger: logger,
		dryRun: dryRun,
	}
}

// SetNodeRisk sets the risk label on a node.
// This triggers Karpenter's Drift mechanism if configured.
func (l *Labeler) SetNodeRisk(ctx context.Context, nodeName string, risk string, reason string) error {
	if risk != RiskLow && risk != RiskHigh {
		return fmt.Errorf("invalid risk value: %s (must be 'low' or 'high')", risk)
	}

	l.logger.Info("setting node risk label",
		"node", nodeName,
		"risk", risk,
		"reason", reason,
		"action", "karpenter_drift_trigger",
	)

	if l.dryRun {
		l.logger.Info("DRY-RUN: would set risk label",
			"node", nodeName,
			"label", fmt.Sprintf("%s=%s", RiskLabel, risk),
		)
		return nil
	}

	// Get current node
	node, err := l.k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	// Update labels
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[RiskLabel] = risk

	// Add reason annotation for debugging
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations["spotvortex.io/risk-reason"] = reason

	// Apply update
	_, err = l.k8s.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node %s: %w", nodeName, err)
	}

	l.logger.Info("node risk label applied",
		"node", nodeName,
		"risk", risk,
	)

	return nil
}

// MarkNodeHighRisk marks a node as high-risk, triggering Karpenter Drift.
func (l *Labeler) MarkNodeHighRisk(ctx context.Context, nodeName, reason string) error {
	return l.SetNodeRisk(ctx, nodeName, RiskHigh, reason)
}

// MarkNodeLowRisk marks a node as low-risk (safe).
func (l *Labeler) MarkNodeLowRisk(ctx context.Context, nodeName, reason string) error {
	return l.SetNodeRisk(ctx, nodeName, RiskLow, reason)
}

// ResetNodeRisk removes the risk label from a node.
// Used when transitioning back to stable market conditions.
func (l *Labeler) ResetNodeRisk(ctx context.Context, nodeName string) error {
	l.logger.Info("resetting node risk label",
		"node", nodeName,
	)

	if l.dryRun {
		l.logger.Info("DRY-RUN: would remove risk label", "node", nodeName)
		return nil
	}

	node, err := l.k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Labels == nil {
		return nil // No labels to remove
	}

	delete(node.Labels, RiskLabel)
	delete(node.Annotations, "spotvortex.io/risk-reason")

	_, err = l.k8s.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node %s: %w", nodeName, err)
	}

	return nil
}

// IsKarpenterInstalled checks if Karpenter CRDs are present in the cluster.
func (l *Labeler) IsKarpenterInstalled(ctx context.Context) bool {
	// Check for NodePool CRD (Karpenter v1+)
	_, err := l.k8s.Discovery().RESTClient().
		Get().
		AbsPath("/apis/karpenter.sh/v1/nodepools").
		DoRaw(ctx)

	if err == nil {
		l.logger.Info("Karpenter detected: enabling Label-Only mode")
		return true
	}

	// Check for v1beta1 (older Karpenter)
	_, err = l.k8s.Discovery().RESTClient().
		Get().
		AbsPath("/apis/karpenter.sh/v1beta1/nodepools").
		DoRaw(ctx)

	if err == nil {
		l.logger.Info("Karpenter (v1beta1) detected: enabling Label-Only mode")
		return true
	}

	l.logger.Debug("Karpenter not detected, using Eviction API mode")
	return false
}
