package controller

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestCheckHighUtilization(t *testing.T) {
	// Create guardrail checker with default settings
	g := &GuardrailChecker{
		highUtilizationThreshold: 0.85,
	}

	tests := []struct {
		name          string
		utilization   float64
		action        Action
		wantApproved  bool
		wantModified  Action
		wantHasReason bool
	}{
		{
			name:          "low utilization - approve any action",
			utilization:   0.50,
			action:        ActionDecrease30,
			wantApproved:  true,
			wantModified:  ActionDecrease30,
			wantHasReason: false,
		},
		{
			name:          "HOLD action always passes",
			utilization:   0.95,
			action:        ActionHold,
			wantApproved:  true,
			wantModified:  ActionHold,
			wantHasReason: false,
		},
		{
			name:          "INCREASE action always passes",
			utilization:   0.95,
			action:        ActionIncrease30,
			wantApproved:  true,
			wantModified:  ActionIncrease30,
			wantHasReason: false,
		},
		{
			name:          "high utilization - downgrade EMERGENCY to DECREASE_30",
			utilization:   0.90,
			action:        ActionEmergencyExit,
			wantApproved:  true,
			wantModified:  ActionDecrease30, // Downgraded
			wantHasReason: true,
		},
		{
			name:          "very high utilization (>95%) - block DECREASE actions",
			utilization:   0.96,
			action:        ActionDecrease30,
			wantApproved:  false,
			wantHasReason: true,
		},
		{
			name:          "high but not extreme (86%) - allow with warning",
			utilization:   0.86,
			action:        ActionDecrease10,
			wantApproved:  true,
			wantModified:  ActionDecrease10,
			wantHasReason: true, // Has warning reason
		},
		{
			name:          "at threshold (85%) - passes",
			utilization:   0.85,
			action:        ActionDecrease30,
			wantApproved:  true,
			wantModified:  ActionDecrease30,
			wantHasReason: false, // No warning when exactly at threshold
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := NodeState{
				ClusterUtilization: tc.utilization,
			}

			result := g.checkHighUtilization(state, tc.action)

			if result.Approved != tc.wantApproved {
				t.Errorf("Approved: got %v, want %v", result.Approved, tc.wantApproved)
			}

			if tc.wantApproved && result.ModifiedAction != tc.wantModified {
				t.Errorf("ModifiedAction: got %v, want %v", result.ModifiedAction, tc.wantModified)
			}

			hasReason := result.Reason != ""
			if hasReason != tc.wantHasReason {
				t.Errorf("HasReason: got %v, want %v (reason: %q)", hasReason, tc.wantHasReason, result.Reason)
			}

			if result.GuardrailName != "high_utilization" {
				t.Errorf("GuardrailName: got %q, want %q", result.GuardrailName, "high_utilization")
			}
		})
	}
}

func TestGuardrailCheckerDefaults(t *testing.T) {
	g := NewGuardrailChecker(nil, nil, 0)

	// Test defaults are set
	if g.clusterFractionLimit != 0.20 {
		t.Errorf("clusterFractionLimit: got %.2f, want 0.20", g.clusterFractionLimit)
	}
	if g.confidenceThreshold != 0.50 {
		t.Errorf("confidenceThreshold: got %.2f, want 0.50", g.confidenceThreshold)
	}
	if g.highUtilizationThreshold != 0.85 {
		t.Errorf("highUtilizationThreshold: got %.2f, want 0.85", g.highUtilizationThreshold)
	}
}

func TestCheckConfidence(t *testing.T) {
	g := &GuardrailChecker{
		confidenceThreshold: 0.50,
	}

	tests := []struct {
		name         string
		confidence   float64
		wantApproved bool
	}{
		{name: "above threshold", confidence: 0.75, wantApproved: true},
		{name: "at threshold", confidence: 0.50, wantApproved: true},
		{name: "below threshold", confidence: 0.49, wantApproved: false},
		{name: "zero confidence", confidence: 0.0, wantApproved: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := NodeState{Confidence: tc.confidence}
			result := g.checkConfidence(state)

			if result.Approved != tc.wantApproved {
				t.Errorf("Approved: got %v, want %v", result.Approved, tc.wantApproved)
			}

			if result.GuardrailName != "low_confidence" {
				t.Errorf("GuardrailName: got %q, want %q", result.GuardrailName, "low_confidence")
			}
		})
	}
}

func TestCheckPDB(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	g := NewGuardrailChecker(k8s, slog.Default(), 0)

	// Create PDB blocking eviction
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb-1", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: 0,
		},
	}
	k8s.PolicyV1().PodDisruptionBudgets("default").Create(context.Background(), pdb, metav1.CreateOptions{})

	// Pod matches PDB
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default", Labels: map[string]string{"app": "foo"}},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
	}
	k8s.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	// Mock Node
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}

	// Action EmergencyExit -> Check PDB
	res, err := g.checkPDB(context.Background(), node, ActionEmergencyExit)
	if err != nil {
		t.Fatalf("checkPDB failed: %v", err)
	}
	if !res.Approved {
		t.Error("expected Approved=true (downgraded), got false")
	}
	if res.ModifiedAction != ActionDecrease30 {
		t.Errorf("expected ModifiedAction=Decrease30, got %v", res.ModifiedAction)
	}
}

func TestCheckClusterFraction(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	g := NewGuardrailChecker(k8s, slog.Default(), 0.2) // 20% limit

	// Create 10 nodes (capacity type spot)
	for i := 0; i < 10; i++ {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("node-%d", i),
				Labels: map[string]string{
					"karpenter.sh/capacity-type": "spot",
				},
			},
		}
		k8s.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	}

	// Check 1 node action. Fraction = 1/10 = 0.1 < 0.2 -> OK
	targetNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}}
	res, err := g.checkClusterFraction(context.Background(), targetNode)
	if err != nil {
		t.Fatalf("checkClusterFraction failed: %v", err)
	}
	if !res.Approved {
		t.Error("0.1 fraction should be approved")
	}

	// Reduce cluster size to 4 nodes. Fraction = 1/4 = 0.25 > 0.2 -> Block
	// Delete 6 nodes
	for i := 4; i < 10; i++ {
		k8s.CoreV1().Nodes().Delete(context.Background(), fmt.Sprintf("node-%d", i), metav1.DeleteOptions{})
	}

	// We need to ensure List reflects deletions. Fake client usually does.
	res, err = g.checkClusterFraction(context.Background(), targetNode)
	if err != nil {
		t.Fatalf("checkClusterFraction failed: %v", err)
	}
	if res.Approved {
		t.Error("0.25 fraction should be blocked")
	}
}

func TestCheckCriticalWorkloads(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	g := NewGuardrailChecker(k8s, slog.Default(), 0)

	// Pod with critical annotation
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "critical-pod",
			Namespace: "kube-system",
			Annotations: map[string]string{
				"spotvortex.io/critical": "true",
			},
		},
		Spec: corev1.PodSpec{NodeName: "node-critical"},
	}
	k8s.CoreV1().Pods("kube-system").Create(context.Background(), pod, metav1.CreateOptions{})

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-critical"}}

	res, err := g.checkCriticalWorkloads(context.Background(), node, ActionEmergencyExit)
	if err != nil {
		t.Fatalf("checkCriticalWorkloads failed: %v", err)
	}
	// Critical pod downgrades Emergency to Decrease30
	// Wait, checkCriticalWorkloads implementation says:
	// if pod.Annotations[AnnotationCritical] == "true" { ... return ... ModifiedAction: ActionDecrease30 }
	if res.ModifiedAction != ActionDecrease30 {
		t.Errorf("expected downgrade to Decrease30, got %v", res.ModifiedAction)
	}

	// Delete critical pod to avoid leakage (fake client might not filter by nodeName correctly in List)
	k8s.CoreV1().Pods("kube-system").Delete(context.Background(), "critical-pod", metav1.DeleteOptions{})

	// Normal pod
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "normal-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "node-normal"},
	}
	k8s.CoreV1().Pods("default").Create(context.Background(), pod2, metav1.CreateOptions{})

	nodeNormal := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-normal"}}

	res, err = g.checkCriticalWorkloads(context.Background(), nodeNormal, ActionEmergencyExit)
	if err != nil {
		t.Fatalf("checkCriticalWorkloads failed: %v", err)
	}
	if !res.Approved {
		t.Error("normal pod should be approved")
	}
	if res.ModifiedAction != ActionEmergencyExit {
		t.Error("normal pod should not downgrade action")
	}
}
