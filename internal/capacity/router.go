package capacity

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
)

// Router routes capacity operations to the correct CapacityManager
// based on per-node provisioner detection.
//
// A cluster may have nodes managed by different provisioners simultaneously.
// The router detects each node's provisioner and dispatches to the right manager.
type Router struct {
	detector *Detector
	managers map[ManagerType]CapacityManager
	logger   *slog.Logger
}

// NewRouter creates a new capacity router with the given managers.
func NewRouter(logger *slog.Logger, managers ...CapacityManager) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	mgrMap := make(map[ManagerType]CapacityManager, len(managers))
	for _, mgr := range managers {
		mgrMap[mgr.Type()] = mgr
	}
	return &Router{
		detector: NewDetector(logger),
		managers: mgrMap,
		logger:   logger,
	}
}

// ManagerForNode returns the CapacityManager for a specific node.
// Returns nil if no manager is registered for the node's provisioner type.
func (r *Router) ManagerForNode(node *corev1.Node) CapacityManager {
	mgrType := r.detector.DetectManager(node)
	if mgrType == ManagerUnknown {
		return nil
	}

	mgr, ok := r.managers[mgrType]
	if !ok {
		// CA and MNG share the same ASG mechanics. If CA manager is registered
		// but MNG is requested (or vice versa), try the other.
		if mgrType == ManagerManagedNodegroup {
			if caMgr, ok := r.managers[ManagerClusterAutoscaler]; ok {
				return caMgr
			}
		}
		if mgrType == ManagerClusterAutoscaler {
			if mngMgr, ok := r.managers[ManagerManagedNodegroup]; ok {
				return mngMgr
			}
		}
		return nil
	}
	return mgr
}

// ManagerForType returns the CapacityManager for a specific manager type.
func (r *Router) ManagerForType(mgrType ManagerType) CapacityManager {
	return r.managers[mgrType]
}

// DetectManagerType returns the detected manager type for a node.
func (r *Router) DetectManagerType(node *corev1.Node) ManagerType {
	return r.detector.DetectManager(node)
}

// PrepareSwapForNode prepares replacement capacity for a specific node.
// It detects the manager type and delegates to the appropriate CapacityManager.
func (r *Router) PrepareSwapForNode(ctx context.Context, node *corev1.Node, pool PoolInfo, direction SwapDirection) (*SwapResult, error) {
	mgr := r.ManagerForNode(node)
	if mgr == nil {
		mgrType := r.detector.DetectManager(node)
		return nil, fmt.Errorf("no capacity manager registered for node %q (detected: %s)", node.Name, mgrType)
	}

	r.logger.Info("routing capacity swap",
		"node", node.Name,
		"manager", mgr.Type(),
		"pool", pool.Name,
		"direction", direction.String(),
	)

	return mgr.PrepareSwap(ctx, pool, direction)
}

// PostDrainCleanupForNode runs post-drain cleanup for a specific node.
func (r *Router) PostDrainCleanupForNode(ctx context.Context, node *corev1.Node, pool PoolInfo) error {
	mgr := r.ManagerForNode(node)
	if mgr == nil {
		return nil // No manager = no cleanup needed
	}
	return mgr.PostDrainCleanup(ctx, node.Name, pool)
}

// HasManager returns true if a manager is registered for the given type.
func (r *Router) HasManager(mgrType ManagerType) bool {
	_, ok := r.managers[mgrType]
	return ok
}

// RegisteredTypes returns the list of registered manager types.
func (r *Router) RegisteredTypes() []ManagerType {
	types := make([]ManagerType, 0, len(r.managers))
	for t := range r.managers {
		types = append(types, t)
	}
	return types
}
