package capacity

import (
	"log/slog"

	corev1 "k8s.io/api/core/v1"
)

// Well-known label keys used for provisioner detection.
const (
	// Karpenter labels
	LabelKarpenterNodePool    = "karpenter.sh/nodepool"
	LabelKarpenterCapacity    = "karpenter.sh/capacity-type"

	// EKS Managed Nodegroup labels
	LabelEKSNodegroup = "eks.amazonaws.com/nodegroup"

	// SpotVortex explicit override
	LabelManagerOverride = "spotvortex.io/manager"

	// SpotVortex pool tag keys for ASG twin discovery
	TagPool         = "spotvortex.io/pool"
	TagCapacityType = "spotvortex.io/capacity-type"
)

// Detector determines which CapacityManager should handle a node
// based on its Kubernetes labels.
//
// Detection priority (first match wins):
//  1. Explicit override: spotvortex.io/manager label
//  2. Karpenter: karpenter.sh/nodepool label present
//  3. EKS Managed Nodegroup: eks.amazonaws.com/nodegroup label present
//  4. Unknown: no recognized provisioner labels
type Detector struct {
	logger *slog.Logger
}

// NewDetector creates a new provisioner detector.
func NewDetector(logger *slog.Logger) *Detector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Detector{logger: logger}
}

// DetectManager returns the ManagerType for a given node.
func (d *Detector) DetectManager(node *corev1.Node) ManagerType {
	if node == nil || node.Labels == nil {
		return ManagerUnknown
	}
	labels := node.Labels

	// Priority 1: Explicit override
	if override, ok := labels[LabelManagerOverride]; ok {
		switch ManagerType(override) {
		case ManagerKarpenter:
			return ManagerKarpenter
		case ManagerClusterAutoscaler:
			return ManagerClusterAutoscaler
		case ManagerManagedNodegroup:
			return ManagerManagedNodegroup
		default:
			d.logger.Warn("unknown manager override, falling through",
				"node", node.Name,
				"override", override,
			)
		}
	}

	// Priority 2: Karpenter
	if _, ok := labels[LabelKarpenterNodePool]; ok {
		return ManagerKarpenter
	}
	// Also detect Karpenter by capacity-type label (older setups without nodepool label)
	if _, ok := labels[LabelKarpenterCapacity]; ok {
		// Only if no EKS nodegroup label (disambiguate)
		if _, hasNG := labels[LabelEKSNodegroup]; !hasNG {
			return ManagerKarpenter
		}
	}

	// Priority 3: EKS Managed Nodegroup
	if _, ok := labels[LabelEKSNodegroup]; ok {
		return ManagerManagedNodegroup
	}

	return ManagerUnknown
}

// DetectManagerForLabels is a convenience function for label maps (without full Node object).
func (d *Detector) DetectManagerForLabels(labels map[string]string) ManagerType {
	if labels == nil {
		return ManagerUnknown
	}
	node := &corev1.Node{}
	node.Labels = labels
	return d.DetectManager(node)
}

// GroupNodesByManager groups a list of nodes by their detected manager type.
func (d *Detector) GroupNodesByManager(nodes []corev1.Node) map[ManagerType][]corev1.Node {
	groups := make(map[ManagerType][]corev1.Node)
	for _, node := range nodes {
		mgr := d.DetectManager(&node)
		groups[mgr] = append(groups[mgr], node)
	}
	return groups
}
