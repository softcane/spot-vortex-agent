// Package karpenter provides NodePool management for SpotVortex actions.
// Implements FALLBACK_OD and RECOVER actions per phase.md lines 332-360.
// No mocks, no fallbacks - production only.
package karpenter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// NodePool GVR for Karpenter v1
var nodePoolGVR = schema.GroupVersionResource{
	Group:    "karpenter.sh",
	Version:  "v1",
	Resource: "nodepools",
}

// CapacityType constants
const (
	CapacityTypeSpot     = "spot"
	CapacityTypeOnDemand = "on-demand"
)

// NodePoolManager manages Karpenter NodePool resources.
type NodePoolManager struct {
	dynamicClient dynamic.Interface
	logger        *slog.Logger
}

// NewNodePoolManager creates a new NodePool manager.
func NewNodePoolManager(dynamicClient dynamic.Interface, logger *slog.Logger) *NodePoolManager {
	return &NodePoolManager{
		dynamicClient: dynamicClient,
		logger:        logger,
	}
}

// SetCapacityTypes updates the NodePool to only allow specified capacity types.
// Used by FALLBACK_OD to set ["on-demand"] and RECOVER to set ["spot", "on-demand"].
func (m *NodePoolManager) SetCapacityTypes(ctx context.Context, poolName string, capacityTypes []string) error {
	if m.dynamicClient == nil {
		return fmt.Errorf("dynamic client not configured")
	}

	m.logger.Info("updating NodePool capacity types",
		"nodepool", poolName,
		"capacity_types", capacityTypes,
	)

	// Build the JSON patch for the requirements
	// Karpenter NodePool structure:
	// spec.template.spec.requirements[].key=karpenter.sh/capacity-type
	patch := buildCapacityTypePatch(capacityTypes)

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = m.dynamicClient.Resource(nodePoolGVR).Patch(
		ctx,
		poolName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch NodePool %s: %w", poolName, err)
	}

	m.logger.Info("NodePool capacity types updated",
		"nodepool", poolName,
		"capacity_types", capacityTypes,
	)

	return nil
}

// FallbackToOnDemand sets the NodePool to only use on-demand instances.
// Per phase.md lines 332-346: FALLBACK_OD action.
func (m *NodePoolManager) FallbackToOnDemand(ctx context.Context, poolName string) error {
	return m.SetCapacityTypes(ctx, poolName, []string{CapacityTypeOnDemand})
}

// RecoverToSpot re-enables spot instances for the NodePool.
// Per phase.md lines 347-360: RECOVER action.
func (m *NodePoolManager) RecoverToSpot(ctx context.Context, poolName string) error {
	return m.SetCapacityTypes(ctx, poolName, []string{CapacityTypeSpot, CapacityTypeOnDemand})
}

// GetCapacityTypes returns the current capacity types for a NodePool.
func (m *NodePoolManager) GetCapacityTypes(ctx context.Context, poolName string) ([]string, error) {
	if m.dynamicClient == nil {
		return nil, fmt.Errorf("dynamic client not configured")
	}
	nodePool, err := m.dynamicClient.Resource(nodePoolGVR).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get NodePool %s: %w", poolName, err)
	}

	// Navigate: spec.template.spec.requirements
	requirements, found, err := unstructured.NestedSlice(nodePool.Object, "spec", "template", "spec", "requirements")
	if err != nil || !found {
		return nil, fmt.Errorf("requirements not found in NodePool %s", poolName)
	}

	for _, req := range requirements {
		reqMap, ok := req.(map[string]interface{})
		if !ok {
			continue
		}

		key, _, _ := unstructured.NestedString(reqMap, "key")
		if key == "karpenter.sh/capacity-type" {
			values, _, _ := unstructured.NestedStringSlice(reqMap, "values")
			return values, nil
		}
	}

	return nil, fmt.Errorf("capacity-type requirement not found in NodePool %s", poolName)
}

// ListNodePools returns all NodePool names in the cluster.
func (m *NodePoolManager) ListNodePools(ctx context.Context) ([]string, error) {
	if m.dynamicClient == nil {
		return nil, fmt.Errorf("dynamic client not configured")
	}
	list, err := m.dynamicClient.Resource(nodePoolGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list NodePools: %w", err)
	}

	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}

	return names, nil
}

// buildCapacityTypePatch creates a JSON merge patch for capacity types.
func buildCapacityTypePatch(capacityTypes []string) map[string]interface{} {
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"requirements": []map[string]interface{}{
						{
							"key":      "karpenter.sh/capacity-type",
							"operator": "In",
							"values":   capacityTypes,
						},
					},
				},
			},
		},
	}
}

// IsKarpenterAvailable checks if Karpenter NodePool CRD exists.
func (m *NodePoolManager) IsKarpenterAvailable(ctx context.Context) bool {
	if m.dynamicClient == nil {
		return false
	}
	_, err := m.dynamicClient.Resource(nodePoolGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// SetWeight updates the NodePool spec.weight to steer provisioning priority.
// Higher weights mean higher priority when multiple NodePools match a pod.
// Per production doc Section 5.4: weights control new provisioning, drains control migration.
// Note: Weight cannot be set on NodePools that use spec.replicas (static pools).
func (m *NodePoolManager) SetWeight(ctx context.Context, poolName string, weight int32) error {
	if m.dynamicClient == nil {
		return fmt.Errorf("dynamic client not configured")
	}

	m.logger.Info("updating NodePool weight",
		"nodepool", poolName,
		"weight", weight,
	)

	patch := buildWeightPatch(weight)

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal weight patch: %w", err)
	}

	_, err = m.dynamicClient.Resource(nodePoolGVR).Patch(
		ctx,
		poolName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch NodePool %s weight: %w", poolName, err)
	}

	m.logger.Info("NodePool weight updated",
		"nodepool", poolName,
		"weight", weight,
	)

	return nil
}

// GetWeight returns the current weight for a NodePool.
// Returns 0 if weight is not set (Karpenter default).
func (m *NodePoolManager) GetWeight(ctx context.Context, poolName string) (int32, error) {
	if m.dynamicClient == nil {
		return 0, fmt.Errorf("dynamic client not configured")
	}
	nodePool, err := m.dynamicClient.Resource(nodePoolGVR).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to get NodePool %s: %w", poolName, err)
	}

	// Navigate: spec.weight
	weight, found, err := unstructured.NestedInt64(nodePool.Object, "spec", "weight")
	if err != nil {
		return 0, fmt.Errorf("failed to read weight from NodePool %s: %w", poolName, err)
	}
	if !found {
		return 0, nil // Weight defaults to 0 in Karpenter
	}

	return int32(weight), nil
}

// SetLimits updates the NodePool spec.limits to cap total resources.
// This provides hard rails on capacity that a NodePool can provision.
// Pass empty strings to remove limits.
func (m *NodePoolManager) SetLimits(ctx context.Context, poolName string, cpu, memory string) error {
	if m.dynamicClient == nil {
		return fmt.Errorf("dynamic client not configured")
	}

	m.logger.Info("updating NodePool limits",
		"nodepool", poolName,
		"cpu", cpu,
		"memory", memory,
	)

	limits := make(map[string]interface{})
	if cpu != "" {
		limits["cpu"] = cpu
	}
	if memory != "" {
		limits["memory"] = memory
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"limits": limits,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal limits patch: %w", err)
	}

	_, err = m.dynamicClient.Resource(nodePoolGVR).Patch(
		ctx,
		poolName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch NodePool %s limits: %w", poolName, err)
	}

	m.logger.Info("NodePool limits updated",
		"nodepool", poolName,
		"cpu", cpu,
		"memory", memory,
	)

	return nil
}

// buildWeightPatch creates a JSON merge patch for weight.
func buildWeightPatch(weight int32) map[string]interface{} {
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"weight": weight,
		},
	}
}

// DisruptionBudget represents a Karpenter disruption budget entry.
// Per production doc Section 2.4: disruption budgets rate-limit voluntary disruptions
// and count nodes being deleted for any reason (including SpotVortex drains).
type DisruptionBudget struct {
	// Nodes is the maximum number of nodes that can be disrupted simultaneously.
	// Can be an absolute number (e.g., "10") or a percentage (e.g., "10%").
	Nodes string
	// Schedule is an optional cron schedule when this budget is active.
	// If empty, the budget is always active.
	Schedule string
	// Reasons limits when this budget applies (e.g., "drifted", "underutilized", "empty").
	// If empty, applies to all reasons.
	Reasons []string
}

// GetDisruptionBudgets returns the disruption budgets for a NodePool.
// Per production doc: "Configure NodePool.spec.disruption.budgets to rate-limit
// Karpenter's voluntary disruptions (drift/emptiness/consolidation)"
func (m *NodePoolManager) GetDisruptionBudgets(ctx context.Context, poolName string) ([]DisruptionBudget, error) {
	if m.dynamicClient == nil {
		return nil, fmt.Errorf("dynamic client not configured")
	}

	nodePool, err := m.dynamicClient.Resource(nodePoolGVR).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get NodePool %s: %w", poolName, err)
	}

	// Navigate: spec.disruption.budgets[]
	budgets, found, err := unstructured.NestedSlice(nodePool.Object, "spec", "disruption", "budgets")
	if err != nil {
		return nil, fmt.Errorf("failed to read disruption budgets from NodePool %s: %w", poolName, err)
	}
	if !found || len(budgets) == 0 {
		// No budgets defined - Karpenter has no limit (default behavior)
		return nil, nil
	}

	result := make([]DisruptionBudget, 0, len(budgets))
	for _, b := range budgets {
		budgetMap, ok := b.(map[string]interface{})
		if !ok {
			continue
		}

		budget := DisruptionBudget{}

		// Parse "nodes" field (required)
		if nodes, found, _ := unstructured.NestedString(budgetMap, "nodes"); found {
			budget.Nodes = nodes
		}

		// Parse "schedule" field (optional cron expression)
		if schedule, found, _ := unstructured.NestedString(budgetMap, "schedule"); found {
			budget.Schedule = schedule
		}

		// Parse "reasons" field (optional string slice)
		if reasons, found, _ := unstructured.NestedStringSlice(budgetMap, "reasons"); found {
			budget.Reasons = reasons
		}

		result = append(result, budget)
	}

	return result, nil
}

// GetEffectiveDisruptionLimit returns the effective node disruption limit for a NodePool.
// This considers the current disruption budgets and returns the maximum number of nodes
// that can be disrupted. Returns -1 if there's no limit.
// Per production doc: "keep drain concurrency below budgets"
func (m *NodePoolManager) GetEffectiveDisruptionLimit(ctx context.Context, poolName string, totalNodes int) (int, error) {
	budgets, err := m.GetDisruptionBudgets(ctx, poolName)
	if err != nil {
		return -1, err
	}

	if len(budgets) == 0 {
		// No budgets = no limit
		return -1, nil
	}

	// Find the most restrictive budget that applies now (ignoring schedule for simplicity)
	// In production, you'd want to evaluate cron schedules to find active budgets
	minLimit := -1
	for _, budget := range budgets {
		if budget.Nodes == "" {
			continue
		}

		limit := parseNodeLimit(budget.Nodes, totalNodes)
		if limit >= 0 {
			if minLimit < 0 || limit < minLimit {
				minLimit = limit
			}
		}
	}

	return minLimit, nil
}

// parseNodeLimit parses a node limit string (e.g., "10" or "10%") and returns
// the absolute number of nodes. Returns -1 if invalid.
func parseNodeLimit(limit string, totalNodes int) int {
	if limit == "" {
		return -1
	}

	// Check for percentage
	if len(limit) > 0 && limit[len(limit)-1] == '%' {
		percentStr := limit[:len(limit)-1]
		var percent int
		if _, err := fmt.Sscanf(percentStr, "%d", &percent); err != nil {
			return -1
		}
		if percent <= 0 {
			return 0
		}
		// Calculate percentage of total nodes
		return (totalNodes * percent) / 100
	}

	// Parse as absolute number
	var count int
	if _, err := fmt.Sscanf(limit, "%d", &count); err != nil {
		return -1
	}
	return count
}
