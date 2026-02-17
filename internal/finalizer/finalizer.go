// Package finalizer implements drain protection for SpotVortex.
//
// Uses the finalizer pattern to ensure nodes are never deleted
// before their replacements are healthy.
//
// Based on: mission_guardrail.md (Finalizer Protection)
package finalizer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// DrainProtectionFinalizer prevents node deletion until safe
	DrainProtectionFinalizer = "spotvortex.io/drain-protection"

	// ReplacementReadyAnnotation marks when replacement is ready
	ReplacementReadyAnnotation = "spotvortex.io/replacement-ready"

	// DrainStartedAnnotation marks when drain was initiated
	DrainStartedAnnotation = "spotvortex.io/drain-started"
)

// Controller manages drain protection finalizers
type Controller struct {
	client kubernetes.Interface
	logger *slog.Logger
	dryRun bool
}

// NewController creates a new finalizer controller
func NewController(client kubernetes.Interface, logger *slog.Logger, dryRun bool) *Controller {
	return &Controller{
		client: client,
		logger: logger,
		dryRun: dryRun,
	}
}

// AddProtection adds the drain protection finalizer to a node
func (c *Controller) AddProtection(ctx context.Context, nodeName string) error {
	c.logger.Info("adding drain protection",
		"node", nodeName,
		"finalizer", DrainProtectionFinalizer,
	)

	if c.dryRun {
		c.logger.Info("DRY-RUN: would add finalizer", "node", nodeName)
		return nil
	}

	node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Check if already has finalizer
	for _, f := range node.Finalizers {
		if f == DrainProtectionFinalizer {
			return nil // Already protected
		}
	}

	// Add finalizer
	node.Finalizers = append(node.Finalizers, DrainProtectionFinalizer)
	node.Annotations[DrainStartedAnnotation] = time.Now().Format(time.RFC3339)

	_, err = c.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to add finalizer: %w", err)
	}

	c.logger.Info("drain protection added", "node", nodeName)
	return nil
}

// MarkReplacementReady marks that the replacement node is healthy
func (c *Controller) MarkReplacementReady(ctx context.Context, nodeName, replacementNode string) error {
	c.logger.Info("marking replacement ready",
		"node", nodeName,
		"replacement", replacementNode,
	)

	if c.dryRun {
		return nil
	}

	node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[ReplacementReadyAnnotation] = replacementNode

	_, err = c.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

// RemoveProtection removes the finalizer after safe handover
func (c *Controller) RemoveProtection(ctx context.Context, nodeName string) error {
	c.logger.Info("removing drain protection", "node", nodeName)

	if c.dryRun {
		c.logger.Info("DRY-RUN: would remove finalizer", "node", nodeName)
		return nil
	}

	node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Remove finalizer
	var newFinalizers []string
	for _, f := range node.Finalizers {
		if f != DrainProtectionFinalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}
	node.Finalizers = newFinalizers

	_, err = c.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}

	c.logger.Info("drain protection removed", "node", nodeName)
	return nil
}

// IsProtected checks if a node has the drain protection finalizer
func (c *Controller) IsProtected(node *corev1.Node) bool {
	for _, f := range node.Finalizers {
		if f == DrainProtectionFinalizer {
			return true
		}
	}
	return false
}

// IsReplacementReady checks if the replacement is marked ready
func (c *Controller) IsReplacementReady(node *corev1.Node) bool {
	if node.Annotations == nil {
		return false
	}
	_, ok := node.Annotations[ReplacementReadyAnnotation]
	return ok
}
