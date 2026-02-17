package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestGetWorkloadPoolFromPoolID(t *testing.T) {
	tests := []struct {
		poolID string
		want   string
	}{
		{"m5.large:us-east-1a", ""},
		{"worker:m5.large:us-east-1a", "worker"},
		{"backend:c5.xlarge:us-west-2b", "backend"},
		{"invalid", ""},
	}

	for _, tc := range tests {
		got := getWorkloadPoolFromPoolID(tc.poolID)
		if got != tc.want {
			t.Errorf("getWorkloadPoolFromPoolID(%q) = %q; want %q", tc.poolID, got, tc.want)
		}
	}
}

func TestGetPoolIDWithExtendedFormat(t *testing.T) {
	c := &Controller{
		karpenterCfg: config.KarpenterConfig{
			UseExtendedPoolID: true,
		},
	}

	got := c.getPoolIDWithExtendedFormat("m5.large", "us-east-1a", "worker")
	want := "worker:m5.large:us-east-1a"
	if got != want {
		t.Errorf("getPoolIDWithExtendedFormat = %q; want %q", got, want)
	}

	c.karpenterCfg.UseExtendedPoolID = false
	got = c.getPoolIDWithExtendedFormat("m5.large", "us-east-1a", "worker")
	want = "m5.large:us-east-1a"
	if got != want {
		t.Errorf("getPoolIDWithExtendedFormat(disabled) = %q; want %q", got, want)
	}
}

func TestIsDaemonSetPod(t *testing.T) {
	owners := []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "rs-1"},
		{Kind: "DaemonSet", Name: "ds-1"},
	}
	if !isDaemonSetPod(owners) {
		t.Error("isDaemonSetPod(DaemonSet) = false; want true")
	}

	owners = []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "rs-1"},
	}
	if isDaemonSetPod(owners) {
		t.Error("isDaemonSetPod(ReplicaSet) = true; want false")
	}
}

func TestNodeHasNamespace(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	c := &Controller{
		k8s:    k8s,
		logger: slog.Default(),
	}

	// Case 1: Pod in target namespace
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "target-ns",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
		},
	}
	k8s.CoreV1().Pods("target-ns").Create(context.Background(), pod1, metav1.CreateOptions{})

	if !c.nodeHasNamespace(context.Background(), "node-1", "target-ns") {
		t.Error("nodeHasNamespace(node-1, target-ns) = false; want true")
	}

	// Case 2: Pod in other namespace
	if c.nodeHasNamespace(context.Background(), "node-1", "other-ns") {
		t.Error("nodeHasNamespace(node-1, other-ns) = true; want false")
	}

	// Case 3: DaemonSet pod should be ignored?
	// The function says: if isDaemonSetPod(owners) { continue }
	// So if ONLY DS pods are present, it should return false.

	podDS := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-ds",
			Namespace: "target-ns",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "ds"},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-2",
		},
	}
	k8s.CoreV1().Pods("target-ns").Create(context.Background(), podDS, metav1.CreateOptions{})

	if c.nodeHasNamespace(context.Background(), "node-2", "target-ns") {
		t.Error("nodeHasNamespace(node-2, target-ns) = true (with only DS pods); want false")
	}
}

func TestFilterExecutableNodes(t *testing.T) {
	// Need to mock nodeInfoMap
	// Since nodeInfoMap calls listing nodes, we can use fake client?
	// Or mock just the map?
	// controller.go 1519: c.nodeInfoMap(ctx)
	// nodeInfoMap uses c.k8s.CoreV1().Nodes().List

	k8s := k8sfake.NewSimpleClientset()
	c := &Controller{
		k8s:    k8s,
		logger: slog.Default(),
	}

	// Create nodes
	createNode(k8s, "node-managed", "spot", "us-east-1a", "m5.large") // Managed, Spot
	createNode(k8s, "node-od", "on-demand", "us-east-1a", "m5.large") // Managed, OD
	// Wait, createNode helper uses "isSpot" label?
	// Let's verify createNode implementation or assume standard labels.
	// standard labels: "karpenter.sh/capacity-type": "spot" / "on-demand"
	// nodeInfoMap reads these.

	// Manually create special case nodes that createNode doesn't cover easily
	nodeUnmanaged := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-unmanaged",
			Labels: map[string]string{
				// No managed label
			},
		},
	}
	k8s.CoreV1().Nodes().Create(context.Background(), nodeUnmanaged, metav1.CreateOptions{})

	nodeControl := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-control",
			Labels: map[string]string{
				"spotvortex.io/managed":                 "true",
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}
	k8s.CoreV1().Nodes().Create(context.Background(), nodeControl, metav1.CreateOptions{})

	assessments := []NodeAssessment{
		{NodeID: "node-managed", Action: inference.ActionDecrease10},   // Should keep (spot, decrease)
		{NodeID: "node-unmanaged", Action: inference.ActionDecrease10}, // Should drop
		{NodeID: "node-control", Action: inference.ActionDecrease10},   // Should drop
		{NodeID: "node-od", Action: inference.ActionDecrease10},        // Should drop (OD cannot decrease)
		{NodeID: "node-od", Action: inference.ActionIncrease10},        // Should keep (OD can increase)
	}

	filtered := c.filterExecutableNodes(context.Background(), assessments)

	// Expected:
	// 1. node-managed (Decrease10) -> Keep
	// 4. node-od (Decrease10) -> Drop
	// 5. node-managed (Increase10) -> Drop
	// 6. node-od (Increase10) -> Keep

	// Counts:
	// node-managed (Decrease) check
	// node-od (Increase) check

	// We handle duplicate NodeIDs in input? Yes.

	count := len(filtered)
	if count != 2 {
		t.Errorf("expected 2 actionable nodes, got %d", count)
		for _, a := range filtered {
			t.Logf("Kept: %s %v", a.NodeID, a.Action)
		}
	}
}

func TestGetPDBForPod(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	gc := NewGuardrailChecker(k8s, slog.Default(), 0.1)

	// Create PDB
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: nil, // mutually exclusive with MaxUnavailable
			MaxUnavailable: &intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: 1,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "foo"},
			},
		},
	}
	k8s.PolicyV1().PodDisruptionBudgets("default").Create(context.Background(), pdb, metav1.CreateOptions{})

	// Pod matching PDB
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "foo"},
		},
	}

	found, err := gc.getPDBForPod(context.Background(), pod)
	if err != nil {
		t.Errorf("getPDBForPod failed: %v", err)
	}
	if found == nil {
		t.Error("expected to find PDB, got nil")
	} else if found.Name != "test-pdb" {
		t.Errorf("expected PDB test-pdb, got %s", found.Name)
	}

	// Pod not matching PDB
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-2",
			Namespace: "default",
			Labels:    map[string]string{"app": "bar"},
		},
	}
	found, err = gc.getPDBForPod(context.Background(), pod2)
	if err != nil {
		t.Errorf("getPDBForPod failed: %v", err)
	}
	if found != nil {
		t.Errorf("expected nil PDB, got %v", found)
	}
}
