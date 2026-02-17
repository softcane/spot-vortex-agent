package karpenter

import (
	"context"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLabeler_SetNodeRisk(t *testing.T) {
	// Setup
	client := fake.NewSimpleClientset()
	labeler := NewLabeler(client, slog.Default(), false)

	nodeName := "worker-1"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	}
	_, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	// Test SetNodeRisk
	err = labeler.SetNodeRisk(context.Background(), nodeName, RiskHigh, "test-reason")
	if err != nil {
		t.Errorf("SetNodeRisk failed: %v", err)
	}

	// Verify
	updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node: %v", err)
	}

	if val, ok := updatedNode.Labels[RiskLabel]; !ok || val != RiskHigh {
		t.Errorf("expected label %s=%s, got %v", RiskLabel, RiskHigh, val)
	}
	if val, ok := updatedNode.Annotations["spotvortex.io/risk-reason"]; !ok || val != "test-reason" {
		t.Errorf("expected annotation reason, got %v", val)
	}

	// Test ResetNodeRisk
	err = labeler.ResetNodeRisk(context.Background(), nodeName)
	if err != nil {
		t.Errorf("ResetNodeRisk failed: %v", err)
	}

	// Verify reset
	updatedNode, err = client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node: %v", err)
	}
	if _, ok := updatedNode.Labels[RiskLabel]; ok {
		t.Error("expected risk label to be removed")
	}
}

func TestLabeler_DryRun(t *testing.T) {
	// Setup
	client := fake.NewSimpleClientset()
	labeler := NewLabeler(client, slog.Default(), true) // DryRun=true

	nodeName := "dry-run-node"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	}
	_, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	// Test
	err = labeler.SetNodeRisk(context.Background(), nodeName, RiskHigh, "reason")
	if err != nil {
		t.Errorf("SetNodeRisk failed in dry-run: %v", err)
	}

	// Verify NO change
	updatedNode, _ := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if len(updatedNode.Labels) > 0 {
		t.Error("expected no labels in dry-run mode")
	}
}

func TestLabeler_Wrappers(t *testing.T) {
	client := fake.NewSimpleClientset()
	labeler := NewLabeler(client, slog.Default(), false)
	nodeName := "wrapper-node"

	_, _ = client.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}, metav1.CreateOptions{})

	if err := labeler.MarkNodeHighRisk(context.Background(), nodeName, "reason"); err != nil {
		t.Error(err)
	}
	// Verify high
	n, _ := client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if n.Labels[RiskLabel] != RiskHigh {
		t.Error("MarkNodeHighRisk failed")
	}

	if err := labeler.MarkNodeLowRisk(context.Background(), nodeName, "reason"); err != nil {
		t.Error(err)
	}
	// Verify low
	n, _ = client.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if n.Labels[RiskLabel] != RiskLow {
		t.Error("MarkNodeLowRisk failed")
	}
}
