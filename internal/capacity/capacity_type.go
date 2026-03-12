package capacity

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// LabelSpotVortexCapacity uses the same key as ASG twin-discovery tags when
	// clusters choose to project capacity type onto nodes.
	LabelSpotVortexCapacity = TagCapacityType

	// LabelEKSCapacityType is set on EKS nodes to describe SPOT vs ON_DEMAND.
	LabelEKSCapacityType = "eks.amazonaws.com/capacityType"

	// LabelNodeLifecycle is a common spot/on-demand lifecycle signal on nodes.
	LabelNodeLifecycle = "node.kubernetes.io/lifecycle"
)

// NormalizeCapacityType converts provider-specific values into the small runtime
// surface SpotVortex reasons about.
func NormalizeCapacityType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return ""
	}
	normalized = strings.ReplaceAll(normalized, "_", "-")
	normalized = strings.ReplaceAll(normalized, " ", "-")

	switch normalized {
	case "spot", "ec2spot":
		return "spot"
	case "on-demand", "ondemand", "ec2ondemand", "ec2-on-demand", "normal":
		return "on-demand"
	case "reserved", "capacity-block", "capacityblock":
		return "reserved"
	default:
		return normalized
	}
}

// CapacityTypeFromLabels resolves a normalized capacity type from well-known node labels.
func CapacityTypeFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}

	for _, key := range []string{
		LabelKarpenterCapacity,
		LabelSpotVortexCapacity,
		LabelEKSCapacityType,
		LabelNodeLifecycle,
	} {
		if value, ok := labels[key]; ok {
			if normalized := NormalizeCapacityType(value); normalized != "" {
				return normalized
			}
		}
	}
	return ""
}

// IsSpotLabels returns true when the node labels clearly identify spot capacity.
func IsSpotLabels(labels map[string]string) bool {
	return CapacityTypeFromLabels(labels) == "spot"
}

// IsSpotNode returns true when the node is clearly labeled as spot capacity.
func IsSpotNode(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	return IsSpotLabels(node.Labels)
}
