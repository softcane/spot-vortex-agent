package capacity

import (
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDetector_DetectManager(t *testing.T) {
	d := NewDetector(slog.Default())

	tests := []struct {
		name     string
		labels   map[string]string
		expected ManagerType
	}{
		{
			name:     "nil labels",
			labels:   nil,
			expected: ManagerUnknown,
		},
		{
			name:     "empty labels",
			labels:   map[string]string{},
			expected: ManagerUnknown,
		},
		{
			name: "karpenter nodepool label",
			labels: map[string]string{
				"karpenter.sh/nodepool":      "core-services-spot",
				"karpenter.sh/capacity-type": "spot",
			},
			expected: ManagerKarpenter,
		},
		{
			name: "karpenter capacity-type only",
			labels: map[string]string{
				"karpenter.sh/capacity-type": "on-demand",
			},
			expected: ManagerKarpenter,
		},
		{
			name: "eks managed nodegroup",
			labels: map[string]string{
				"eks.amazonaws.com/nodegroup": "my-nodegroup",
			},
			expected: ManagerManagedNodegroup,
		},
		{
			name: "eks nodegroup with karpenter capacity type (MNG wins)",
			labels: map[string]string{
				"eks.amazonaws.com/nodegroup": "my-nodegroup",
				"karpenter.sh/capacity-type":  "spot",
			},
			expected: ManagerManagedNodegroup,
		},
		{
			name: "explicit override karpenter",
			labels: map[string]string{
				"spotvortex.io/manager":       "karpenter",
				"eks.amazonaws.com/nodegroup": "my-nodegroup",
			},
			expected: ManagerKarpenter,
		},
		{
			name: "explicit override cluster-autoscaler",
			labels: map[string]string{
				"spotvortex.io/manager": "cluster-autoscaler",
			},
			expected: ManagerClusterAutoscaler,
		},
		{
			name: "explicit override managed-nodegroup",
			labels: map[string]string{
				"spotvortex.io/manager": "managed-nodegroup",
			},
			expected: ManagerManagedNodegroup,
		},
		{
			name: "explicit override unknown falls through to karpenter",
			labels: map[string]string{
				"spotvortex.io/manager":  "bogus",
				"karpenter.sh/nodepool": "some-pool",
			},
			expected: ManagerKarpenter,
		},
		{
			name: "no recognized labels",
			labels: map[string]string{
				"app": "nginx",
			},
			expected: ManagerUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: tc.labels,
				},
			}
			got := d.DetectManager(node)
			if got != tc.expected {
				t.Errorf("DetectManager() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestDetector_NilNode(t *testing.T) {
	d := NewDetector(slog.Default())
	got := d.DetectManager(nil)
	if got != ManagerUnknown {
		t.Errorf("DetectManager(nil) = %q, want %q", got, ManagerUnknown)
	}
}

func TestDetector_GroupNodesByManager(t *testing.T) {
	d := NewDetector(slog.Default())

	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "karpenter-node-1",
				Labels: map[string]string{"karpenter.sh/nodepool": "pool-spot"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "karpenter-node-2",
				Labels: map[string]string{"karpenter.sh/nodepool": "pool-od"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mng-node-1",
				Labels: map[string]string{"eks.amazonaws.com/nodegroup": "my-ng"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "ca-node-1",
				Labels: map[string]string{"spotvortex.io/manager": "cluster-autoscaler"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "unknown-node",
				Labels: map[string]string{"app": "nginx"},
			},
		},
	}

	groups := d.GroupNodesByManager(nodes)

	if len(groups[ManagerKarpenter]) != 2 {
		t.Errorf("expected 2 karpenter nodes, got %d", len(groups[ManagerKarpenter]))
	}
	if len(groups[ManagerManagedNodegroup]) != 1 {
		t.Errorf("expected 1 MNG node, got %d", len(groups[ManagerManagedNodegroup]))
	}
	if len(groups[ManagerClusterAutoscaler]) != 1 {
		t.Errorf("expected 1 CA node, got %d", len(groups[ManagerClusterAutoscaler]))
	}
	if len(groups[ManagerUnknown]) != 1 {
		t.Errorf("expected 1 unknown node, got %d", len(groups[ManagerUnknown]))
	}
}
