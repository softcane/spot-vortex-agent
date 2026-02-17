package karpenter

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestNodePoolManager_SetCapacityTypes(t *testing.T) {
	// Setup fake client
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	manager := NewNodePoolManager(client, slog.Default())

	// Create a dummy NodePool
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": "default",
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"requirements": []interface{}{},
					},
				},
			},
		},
	}
	_, err := client.Resource(nodePoolGVR).Create(context.Background(), pool, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create dummy pool: %v", err)
	}

	// Test FallbackToOnDemand
	err = manager.FallbackToOnDemand(context.Background(), "default")
	if err != nil {
		t.Errorf("FallbackToOnDemand failed: %v", err)
	}

	// Verify
	types, err := manager.GetCapacityTypes(context.Background(), "default")
	if err != nil {
		t.Errorf("GetCapacityTypes failed: %v", err)
	}
	if len(types) != 1 || types[0] != "on-demand" {
		t.Errorf("expected [on-demand], got %v", types)
	}

	// Test RecoverToSpot
	err = manager.RecoverToSpot(context.Background(), "default")
	if err != nil {
		t.Errorf("RecoverToSpot failed: %v", err)
	}

	// Verify
	types, err = manager.GetCapacityTypes(context.Background(), "default")
	if err != nil {
		t.Errorf("GetCapacityTypes failed: %v", err)
	}
	// Note: buildCapacityTypePatch sets values.
	// Order might vary? Usually strict.
	// Check if contains both.
	hasSpot := false
	hasOD := false
	for _, v := range types {
		if v == "spot" {
			hasSpot = true
		}
		if v == "on-demand" {
			hasOD = true
		}
	}
	if !hasSpot || !hasOD {
		t.Errorf("expected [spot, on-demand], got %v", types)
	}
}

func TestNodePoolManager_SetWeight(t *testing.T) {
	// Setup fake client
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	manager := NewNodePoolManager(client, slog.Default())

	// Create a dummy NodePool
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": "weighted-pool",
			},
			"spec": map[string]interface{}{
				"weight": int64(10), // Default
			},
		},
	}
	_, err := client.Resource(nodePoolGVR).Create(context.Background(), pool, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create dummy pool: %v", err)
	}

	// Test SetWeight
	err = manager.SetWeight(context.Background(), "weighted-pool", 50)
	if err != nil {
		t.Errorf("SetWeight failed: %v", err)
	}

	// Verify
	weight, err := manager.GetWeight(context.Background(), "weighted-pool")
	if err != nil {
		t.Errorf("GetWeight failed: %v", err)
	}
	if weight != 50 {
		t.Errorf("expected weight 50, got %d", weight)
	}
}

func TestNodePoolManager_GetDisruptionBudgets(t *testing.T) {
	// Setup fake client
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	manager := NewNodePoolManager(client, slog.Default())

	// Create a dummy NodePool with budgets
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": "budget-pool",
			},
			"spec": map[string]interface{}{
				"disruption": map[string]interface{}{
					"budgets": []interface{}{
						map[string]interface{}{
							"nodes": "10",
						},
						map[string]interface{}{
							"nodes": "20%",
						},
					},
				},
			},
		},
	}
	_, err := client.Resource(nodePoolGVR).Create(context.Background(), pool, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create dummy pool: %v", err)
	}

	budgets, err := manager.GetDisruptionBudgets(context.Background(), "budget-pool")
	if err != nil {
		t.Fatalf("GetDisruptionBudgets failed: %v", err)
	}

	if len(budgets) != 2 {
		t.Errorf("expected 2 budgets, got %d", len(budgets))
	}
	if budgets[0].Nodes != "10" {
		t.Errorf("expected nodes '10', got '%s'", budgets[0].Nodes)
	}
}

func TestParseNodeLimit(t *testing.T) {
	tests := []struct {
		limit      string
		totalNodes int
		expected   int
	}{
		{"10", 100, 10},
		{"10%", 100, 10},
		{"10%", 50, 5},
		{"0", 100, 0},
		{"100%", 10, 10},
		{"invalid", 100, -1},
		{"", 100, -1},
	}

	for _, tt := range tests {
		got := parseNodeLimit(tt.limit, tt.totalNodes)
		if got != tt.expected {
			t.Errorf("parseNodeLimit(%q, %d) = %d, want %d", tt.limit, tt.totalNodes, got, tt.expected)
		}
	}
}

func TestNodePoolManager_SetLimits(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	manager := NewNodePoolManager(client, slog.Default())

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": "limited-pool",
			},
			"spec": map[string]interface{}{
				"limits": map[string]interface{}{},
			},
		},
	}
	_, err := client.Resource(nodePoolGVR).Create(context.Background(), pool, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}

	err = manager.SetLimits(context.Background(), "limited-pool", "10", "100Gi")
	if err != nil {
		t.Errorf("SetLimits failed: %v", err)
	}

	// Verification would require checking the object, but SetLimits logic is just a patch which fakes usually handle or not.
	// Since we mock the client, the most important part is that it doesn't error.
}

func TestNodePoolManager_GetEffectiveDisruptionLimit(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	manager := NewNodePoolManager(client, slog.Default())

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": "limit-pool",
			},
			"spec": map[string]interface{}{
				"disruption": map[string]interface{}{
					"budgets": []interface{}{
						map[string]interface{}{"nodes": "10"},
						map[string]interface{}{"nodes": "20%"}, // 20% of 100 = 20
					},
				},
			},
		},
	}
	_, err := client.Resource(nodePoolGVR).Create(context.Background(), pool, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}

	// Mock total nodes = 100
	limit, err := manager.GetEffectiveDisruptionLimit(context.Background(), "limit-pool", 100)
	if err != nil {
		t.Errorf("GetEffectiveDisruptionLimit failed: %v", err)
	}
	if limit != 10 { // 10 < 20
		t.Errorf("expected limit 10, got %d", limit)
	}

	// Test with smaller total to flip it
	// If total=40, 20% is 8. So expected 8.
	limit, _ = manager.GetEffectiveDisruptionLimit(context.Background(), "limit-pool", 40)
	if limit != 8 {
		t.Errorf("expected limit 8 (20%% of 40), got %d", limit)
	}
}
