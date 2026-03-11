package collector

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// MockUtilizationProvider implements UtilizationProvider
type MockUtilizationProvider struct {
	Util map[string]float64
}

func (m *MockUtilizationProvider) GetPoolUtilization(ctx context.Context) (map[string]float64, error) {
	return m.Util, nil
}

func TestCollector_Collect(t *testing.T) {
	// Setup Fake Client
	client := fake.NewSimpleClientset()
	logger := slog.Default()
	collector := NewCollector(client, logger)

	// Inject Mock Util Provider
	collector.SetUtilizationProvider(&MockUtilizationProvider{
		Util: map[string]float64{"m5.large:us-east-1a": 0.3},
	})

	// 1. Create Nodes
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"topology.kubernetes.io/zone":      "us-east-1a",
				"node.kubernetes.io/instance-type": "m5.large",
			},
		},
	}
	_, _ = client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})

	// 2. Create PDB (Restricted)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restricted-pdb",
			Namespace: "default",
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			CurrentHealthy: 1,
			DesiredHealthy: 1, // Restricted
		},
	}
	_, _ = client.PolicyV1().PodDisruptionBudgets("default").Create(context.Background(), pdb, metav1.CreateOptions{})

	// 3. Create Pods
	// Pod 1: High Priority, Restricted via Namespace, Started 10s ago
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Second))
	readyTime := metav1.NewTime(time.Now().Add(-5 * time.Second))

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "default",
			Annotations: map[string]string{
				"spotvortex.io/migration-tier": "0", // Critical -> Priority 1.0
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"), // Weight 0.1
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			StartTime: &startTime,
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: readyTime,
				},
			},
		},
	}
	_, _ = client.CoreV1().Pods("default").Create(context.Background(), pod1, metav1.CreateOptions{})

	// Run Collect
	metrics, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	// Verify
	poolID := "m5.large:us-east-1a"
	features, ok := metrics.PoolFeatures[poolID]
	if !ok {
		t.Fatalf("expected pool %s not found", poolID)
	}

	// Startup Time: ~5 seconds (Ready - Start)
	if features.PodStartupTime < 4.9 || features.PodStartupTime > 5.1 {
		t.Errorf("expected startup time ~5.0, got %f", features.PodStartupTime)
	}

	// Priority: 1.0 (Critical from annotation)
	if features.PriorityScore != 1.0 {
		t.Errorf("expected priority 1.0, got %f", features.PriorityScore)
	}
	if !features.HasCriticalPod {
		t.Error("expected HasCriticalPod=true")
	}

	// Cluster Util
	if features.ClusterUtilization != 0.3 {
		t.Errorf("expected utilization 0.3, got %f", features.ClusterUtilization)
	}
}

func TestGetExtendedPoolID(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"topology.kubernetes.io/zone":      "us-west-2a",
				"node.kubernetes.io/instance-type": "c5.xlarge",
				"spotvortex.io/pool":               "ml-training",
			},
		},
	}

	id := GetExtendedPoolID(node)
	expected := "ml-training:c5.xlarge:us-west-2a"
	if id != expected {
		t.Errorf("expected %s, got %s", expected, id)
	}

	// Test fallback
	node.Labels["spotvortex.io/pool"] = ""
	id = GetExtendedPoolID(node)
	expected = "c5.xlarge:us-west-2a"
	if id != expected {
		t.Errorf("expected %s, got %s", expected, id)
	}
}

func TestCollector_CollectPoolSafetyVector(t *testing.T) {
	client := fake.NewSimpleClientset()
	logger := slog.Default()
	collector := NewCollector(client, logger)
	collector.SetUtilizationProvider(&MockUtilizationProvider{
		Util: map[string]float64{
			"m5.large:us-east-1a": 0.5,
			"m5.large:us-east-1b": 0.5,
		},
	})

	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-spot-a",
				Labels: map[string]string{
					"topology.kubernetes.io/zone":      "us-east-1a",
					"node.kubernetes.io/instance-type": "m5.large",
					"karpenter.sh/capacity-type":       "spot",
					WorkloadPoolLabel:                  "api",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-od-a",
				Labels: map[string]string{
					"topology.kubernetes.io/zone":      "us-east-1a",
					"node.kubernetes.io/instance-type": "m5.large",
					"karpenter.sh/capacity-type":       "on-demand",
					WorkloadPoolLabel:                  "api",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-spot-b",
				Labels: map[string]string{
					"topology.kubernetes.io/zone":      "us-east-1b",
					"node.kubernetes.io/instance-type": "m5.large",
					"karpenter.sh/capacity-type":       "spot",
					WorkloadPoolLabel:                  "api",
				},
			},
		},
	}
	for _, node := range nodes {
		if _, err := client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create node %s: %v", node.Name, err)
		}
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-pdb",
			Namespace: "default",
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "api"},
			},
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			CurrentHealthy:     1,
			DesiredHealthy:     1,
			DisruptionsAllowed: 0,
		},
	}
	if _, err := client.PolicyV1().PodDisruptionBudgets("default").Create(context.Background(), pdb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pdb: %v", err)
	}

	startTime := metav1.NewTime(time.Now().Add(-20 * time.Second))
	readyTime := metav1.NewTime(time.Now().Add(-10 * time.Second))

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "critical-api",
				Namespace: "default",
				Labels: map[string]string{
					"app": "api",
				},
				Annotations: map[string]string{
					AnnotationMigrationTier: "0",
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-spot-a",
				Containers: []corev1.Container{{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				}},
			},
			Status: corev1.PodStatus{
				StartTime: &startTime,
				Conditions: []corev1.PodCondition{{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: readyTime,
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stateful-db",
				Namespace: "default",
				Labels: map[string]string{
					"app": "db",
				},
				OwnerReferences: []metav1.OwnerReference{{
					Kind: "StatefulSet",
					Name: "db",
				}},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-spot-a",
				Containers: []corev1.Container{{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker",
				Namespace: "default",
				Labels: map[string]string{
					"app": "worker",
				},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-od-a",
				Containers: []corev1.Container{{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"),
						},
					},
				}},
			},
		},
	}
	for _, pod := range pods {
		if _, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod %s: %v", pod.Name, err)
		}
	}

	metrics, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	features, ok := metrics.PoolFeatures["api:m5.large:us-east-1a"]
	if !ok {
		t.Fatalf("expected extended pool features to be present")
	}

	ps := features.PoolSafety
	if ps.CriticalServiceSpotConcentration != 1.0 {
		t.Fatalf("expected critical concentration 1.0, got %.2f", ps.CriticalServiceSpotConcentration)
	}
	if ps.MinPDBSlackIfOneNodeLost != -1.0 {
		t.Fatalf("expected one-node pdb slack -1.0, got %.2f", ps.MinPDBSlackIfOneNodeLost)
	}
	if ps.StatefulPodFraction < 0.30 || ps.StatefulPodFraction > 0.35 {
		t.Fatalf("expected stateful fraction about 1/3, got %.2f", ps.StatefulPodFraction)
	}
	if ps.EvictablePodFraction < 0.65 || ps.EvictablePodFraction > 0.70 {
		t.Fatalf("expected evictable fraction about 2/3, got %.2f", ps.EvictablePodFraction)
	}
	if ps.SpareODHeadroomNodes != 0.5 {
		t.Fatalf("expected spare OD headroom 0.5, got %.2f", ps.SpareODHeadroomNodes)
	}
	if ps.ZoneDiversificationScore != 0.5 {
		t.Fatalf("expected zone diversification score 0.5, got %.2f", ps.ZoneDiversificationScore)
	}
	if ps.RecoveryBudgetViolationRisk < 0.85 {
		t.Fatalf("expected high recovery risk, got %.2f", ps.RecoveryBudgetViolationRisk)
	}
	if ps.SafeMaxSpotRatio > 0.10 {
		t.Fatalf("expected safe max spot ratio <= 0.10, got %.2f", ps.SafeMaxSpotRatio)
	}
}

func TestParseHoursDuration(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"10h", 10.0},
		{"0.5h", 0.5},
		{"24", 24.0},
		{"invalid", 0.0},
		{"", 0.0},
	}

	for _, tt := range tests {
		got := parseHoursDuration(tt.input)
		if got != tt.want {
			t.Errorf("parseHoursDuration(%q) = %f, want %f", tt.input, got, tt.want)
		}
	}
}
