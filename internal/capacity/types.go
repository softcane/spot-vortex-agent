// Package capacity provides a unified interface for managing node capacity
// across different Kubernetes provisioners: Karpenter, Cluster Autoscaler,
// and EKS Managed Nodegroups.
//
// Design: integration_strategy.md Section 6 - CapacityManager abstraction.
// A cluster may use multiple provisioners simultaneously (e.g., some pools
// on Karpenter, some on Cluster Autoscaler). The controller routes per-node
// based on detected provisioner labels.
package capacity

import (
	"context"
	"time"
)

// ManagerType identifies the provisioner managing a node's lifecycle.
type ManagerType string

const (
	// ManagerKarpenter indicates nodes managed by Karpenter NodePools.
	// Detection: node has karpenter.sh/nodepool label.
	// Swap strategy: steer NodePool weights, then drain (Karpenter provisions replacement).
	ManagerKarpenter ManagerType = "karpenter"

	// ManagerClusterAutoscaler indicates nodes in ASGs managed by Cluster Autoscaler.
	// Detection: node has cluster-autoscaler tags or explicit spotvortex.io/manager=cluster-autoscaler.
	// Swap strategy: Twin ASG - scale up twin, wait for Ready, drain, scale down source.
	ManagerClusterAutoscaler ManagerType = "cluster-autoscaler"

	// ManagerManagedNodegroup indicates nodes in EKS Managed Nodegroups.
	// Detection: node has eks.amazonaws.com/nodegroup label.
	// Swap strategy: Same Twin ASG workflow as Cluster Autoscaler (both use ASGs).
	ManagerManagedNodegroup ManagerType = "managed-nodegroup"

	// ManagerUnknown indicates the provisioner could not be detected.
	// Nodes with unknown manager are skipped for capacity operations.
	ManagerUnknown ManagerType = "unknown"
)

// SwapDirection indicates the direction of a capacity swap.
type SwapDirection int

const (
	// SwapToOnDemand replaces Spot capacity with On-Demand (risk mitigation).
	SwapToOnDemand SwapDirection = iota

	// SwapToSpot replaces On-Demand capacity with Spot (cost optimization).
	SwapToSpot
)

func (d SwapDirection) String() string {
	switch d {
	case SwapToOnDemand:
		return "spot->on-demand"
	case SwapToSpot:
		return "on-demand->spot"
	default:
		return "unknown"
	}
}

// PoolInfo describes a node pool for capacity operations.
type PoolInfo struct {
	// Name is the workload pool name (e.g., "web-backend").
	Name string

	// Zone is the availability zone (e.g., "us-east-1a").
	Zone string

	// InstanceType is the dominant instance type in the pool.
	InstanceType string
}

// SwapResult contains the outcome of a capacity swap preparation.
type SwapResult struct {
	// Ready indicates replacement capacity is available.
	Ready bool

	// ReplacementNodeName is the name of the new node (for ASG swaps).
	// Empty for Karpenter (Karpenter provisions asynchronously after drain).
	ReplacementNodeName string

	// Duration is how long the preparation took.
	Duration time.Duration
}

// CapacityManager provides a unified interface for managing node capacity
// across different Kubernetes provisioners.
//
// Contract:
//   - PrepareSwap MUST be called before any drain to ensure replacement capacity.
//   - PostDrainCleanup SHOULD be called after a successful drain.
//   - Implementations must be safe for concurrent use.
type CapacityManager interface {
	// Type returns the provisioner type this manager handles.
	Type() ManagerType

	// PrepareSwap ensures replacement capacity will be available for the given pool.
	//
	// For Karpenter: steers NodePool weights so next provisioned node matches direction.
	//   This is fast (API patch) and non-blocking.
	//
	// For ASG (CA/MNG): executes the Twin ASG workflow:
	//   1. Scale up the twin ASG (e.g., if direction=SwapToOnDemand, scale up ASG-OD)
	//   2. Wait for new node to become Ready
	//   3. Return once replacement capacity is confirmed
	//   This is slow (minutes) and blocking.
	//
	// Returns error if replacement capacity cannot be provisioned (e.g., quota exceeded, timeout).
	PrepareSwap(ctx context.Context, pool PoolInfo, direction SwapDirection) (*SwapResult, error)

	// PostDrainCleanup is called after a node has been successfully drained.
	//
	// For Karpenter: typically a no-op (Karpenter manages node lifecycle).
	// For ASG: terminates the drained instance and decrements source ASG desired capacity.
	PostDrainCleanup(ctx context.Context, nodeName string, pool PoolInfo) error

	// IsAvailable checks if this manager can operate in the current cluster.
	// For Karpenter: checks if NodePool CRD exists.
	// For ASG: checks if ASG API is accessible and twin ASGs are discoverable.
	IsAvailable(ctx context.Context) bool
}
