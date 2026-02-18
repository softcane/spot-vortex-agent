package controller

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/collector"
	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/karpenter"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// MockCloudProvider implements cloudapi.CloudProvider

type MockCloudProvider struct {
	DryRun bool
}

func (m *MockCloudProvider) GetInstanceType(ctx context.Context, instanceID string) (string, error) {
	return "m5.large", nil
}
func (m *MockCloudProvider) GetZone(ctx context.Context, instanceID string) (string, error) {
	return "us-east-1a", nil
}
func (m *MockCloudProvider) IsSpot(ctx context.Context, instanceID string) (bool, error) {
	return true, nil
}
func (m *MockCloudProvider) IsDryRun() bool {
	return m.DryRun
}
func (m *MockCloudProvider) Drain(ctx context.Context, req cloudapi.DrainRequest) (*cloudapi.DrainResult, error) {
	return &cloudapi.DrainResult{Success: true, NodeID: req.NodeID}, nil
}
func (m *MockCloudProvider) Provision(ctx context.Context, req cloudapi.ProvisionRequest) (*cloudapi.ProvisionResult, error) {
	return &cloudapi.ProvisionResult{InstanceID: "i-new"}, nil
}

// MockPriceProvider implements cloudapi.PriceProvider
type MockPriceProvider struct {
	PriceData cloudapi.SpotPriceData
}

func (m *MockPriceProvider) GetSpotPrice(ctx context.Context, instanceType, zone string) (cloudapi.SpotPriceData, error) {
	return m.PriceData, nil
}
func (m *MockPriceProvider) GetOnDemandPrice(ctx context.Context, instanceType, zone string) (float64, error) {
	return m.PriceData.OnDemandPrice, nil
}

// MockMetricsClient allows mocking Prometheus responses
// Note: The real client uses the Prometheus HTTP API, so we might need to mock the *metrics.Client differently
// or avoid calling methods that hit the network.
// In controller.go, fetchNodeMetrics calls c.prom.GetNodeMetrics(ctx).
// To test this without a real Prometheus, we need to inject a mockable/fake client or skip that step.
// Since metrics.Client is a struct, we can't interface mock it easily unless we refactor.
// However, fetchNodeMetrics checks c.useSyntheticMetrics. If true, it uses c.syntheticNodeMetrics(ctx).
// So we can enable synthetic metrics for testing!

func TestController_Reconcile_DryRun(t *testing.T) {
	// Setup Fake K8s
	k8sClient := k8sfake.NewSimpleClientset()
	dynClient := fake.NewSimpleDynamicClient(runtime.NewScheme())

	// Create generic nodes
	createNode(k8sClient, "node-1", "spot", "us-east-1a", "m5.large")

	// Dependencies
	logger := slog.Default()

	// Use relative paths to real ONNX models and local dummy PySR equations
	// PySR paths are hardcoded in NewInferenceEngine to "models/pysr/..."
	// which corresponds to internal/controller/models/pysr/... in this test context.
	// We created those dummy files.
	inf, err := inference.NewInferenceEngine(inference.EngineConfig{
		TFTModelPath:       "../../models/tft.onnx",
		RLModelPath:        "../../models/rl_policy.onnx",
		Logger:             logger,
		RequireRuntimeHead: false, // Don't enforce runtime head for older models
	})
	if err != nil {
		t.Logf("Failed to load inference engine (skipping Reconcile test): %v", err)
		t.Skip("Skipping Reconcile test due to missing/invalid models")
		return
	}
	// defer inf.Close() // InferenceEngine doesn't have Close? It does!
	defer inf.Close()

	// Mock Cloud & Price
	cloud := &MockCloudProvider{DryRun: true}
	price := &MockPriceProvider{
		PriceData: cloudapi.SpotPriceData{
			CurrentPrice:  0.2,
			OnDemandPrice: 1.0,
			PriceHistory:  []float64{0.2, 0.2, 0.2},
		},
	}

	// Config
	cfg := Config{
		Cloud:               cloud,
		PriceProvider:       price,
		K8sClient:           k8sClient,
		DynamicClient:       dynClient,
		Inference:           inf,
		PrometheusClient:    &metrics.Client{}, // Nil pointer panic if we call real methods
		Logger:              logger,
		RiskThreshold:       0.8,
		MaxDrainRatio:       0.1,
		ReconcileInterval:   10 * time.Second,
		ConfidenceThreshold: 0.5,
		Karpenter: config.KarpenterConfig{
			Enabled: false,
		},
	}

	// Use synthetic metrics to bypass Prometheus (prices come from MockPriceProvider)
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	ctrl, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	// Run Reconcile
	ctx := context.Background()
	// This will use synthetic metrics and run real inference on those metrics!
	err = ctrl.Reconcile(ctx)
	if err != nil {
		t.Errorf("Reconcile failed: %v", err)
	}
}

func TestController_ApplyDrainLimit(t *testing.T) {
	ctrl := &Controller{
		maxDrainRatio: 0.1, // 10%
		logger:        slog.Default(),
	}

	// 100 nodes, 20 at risk
	assessments := make([]NodeAssessment, 20)
	for i := range assessments {
		assessments[i] = NodeAssessment{
			NodeID:        fmt.Sprintf("node-%d", i),
			Action:        inference.ActionDecrease30,
			CapacityScore: 0.5,
		}
	}

	// Make one Emergency (higher priority)
	assessments[19].Action = inference.ActionEmergencyExit
	assessments[19].CapacityScore = 0.9

	limited := ctrl.applyDrainLimit(context.Background(), assessments, 100)

	// Max drain = 100 * 0.1 = 10
	if len(limited) != 10 {
		t.Errorf("expected 10 nodes, got %d", len(limited))
	}

	// Ensure Emergency node is included (should be sorted to top)
	foundEmergency := false
	for _, a := range limited {
		if a.Action == inference.ActionEmergencyExit {
			foundEmergency = true
			break
		}
	}
	if !foundEmergency {
		t.Error("expected emergency action to be prioritized")
	}
}

func TestController_FilterActionableNodes(t *testing.T) {
	ctrl := &Controller{
		confidenceThreshold: 0.5,
		riskThreshold:       0.8,
		logger:              slog.Default(),
	}

	tests := []struct {
		name       string
		assessment NodeAssessment
		wantKeep   bool
		wantAction inference.Action // In case it changes (override)
	}{
		{
			name: "high confidence hold",
			assessment: NodeAssessment{
				Action:     inference.ActionHold,
				Confidence: 0.9,
			},
			wantKeep: false,
		},
		{
			name: "low confidence action",
			assessment: NodeAssessment{
				Action:     inference.ActionDecrease30,
				Confidence: 0.2, // Below 0.5
			},
			wantKeep: false,
		},
		{
			name: "high confidence action",
			assessment: NodeAssessment{
				Action:     inference.ActionDecrease30,
				Confidence: 0.9,
			},
			wantKeep:   true,
			wantAction: inference.ActionDecrease30,
		},
		{
			name: "risk threshold exceeded (TFT override)",
			assessment: NodeAssessment{
				Action:        inference.ActionHold,
				Confidence:    0.9,
				CapacityScore: 0.9, // Above 0.8
			},
			wantKeep:   true,
			wantAction: inference.ActionEmergencyExit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := []NodeAssessment{tc.assessment}
			got := ctrl.filterActionableNodes(input)

			if tc.wantKeep {
				if len(got) != 1 {
					t.Errorf("expected to keep node, got %d", len(got))
				} else if got[0].Action != tc.wantAction {
					t.Errorf("expected action %v, got %v", tc.wantAction, got[0].Action)
				}
			} else {
				if len(got) != 0 {
					t.Errorf("expected to drop node, got %d", len(got))
				}
			}
		})
	}
}

// Helpers

func createNode(client *k8sfake.Clientset, name, capType, zone, instanceType string) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"karpenter.sh/capacity-type":       capType,
				"topology.kubernetes.io/zone":      zone,
				"node.kubernetes.io/instance-type": instanceType,
				"spotvortex.io/managed":            "true",
			},
		},
	}
	client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
}

// Fake Dynamic Client helper if needed (not used deeply yet)
func setupFakeDynamic() *fake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return fake.NewSimpleDynamicClient(scheme)
}

// Additional test for getKarpenterDisruptionLimit
func TestGetKarpenterDisruptionLimit(t *testing.T) {
	// Need to mock NodePoolManager behavior.
	// Since NodePoolManager uses dynamic client, we can simulate responses if we dig deep,
	// but might be easier to just skip deep mocking of dynamic client for now
	// and verify logic when mgr is nil or returns errors.
	ctrl := &Controller{
		logger: slog.Default(),
	}
	limit := ctrl.getKarpenterDisruptionLimit(context.Background(), nil, 100)
	if limit != -1 {
		t.Errorf("expected -1 when mgr is nil, got %d", limit)
	}
}

func TestCalculatePoolDrainCount(t *testing.T) {
	ctrl := &Controller{
		maxDrainRatio: 0.1,
		logger:        slog.Default(),
		poolNodeCounts: map[string]*poolCount{
			"p1": {total: 100, spot: 50},
		},
		currentSpotRatio: map[string]float64{
			"p1": 0.5,
		},
		targetSpotRatio: map[string]float64{
			"p1": 0.5,
		},
	}

	// Increase 10% -> Target 0.6 -> Diff 0.1 -> Need 10 drains
	// But wait? Increase means MORE spot.
	// Current 0.5, Target 0.6.
	// Delta is +0.1.
	// Wait, draining Spot nodes reduces spot ratio? No, usually we drain OD to replace with Spot?
	// Ah, SpotVortex usually drains "at risk" nodes.
	// If action is INCREASE_SPOT, we want MORE spot.
	// If we drain OD nodes, Karpenter might launch Spot?
	// Let's check calculatePoolDrainCount logic:
	// delta = +0.10
	// newTarget = 0.6
	// diff = 0.1
	// need = ceil(0.1 * 100) = 10.
	// Logic just calculates magnitude of change needed.

	// Case 1: Increase 10%
	count := ctrl.calculatePoolDrainCount("p1", inference.ActionIncrease10)
	if count != 10 {
		t.Errorf("ActionIncrease10: expected 10, got %d", count)
	}

	// Case 2: Max drain limit
	// current 0.5, target 0.5. Action Decrease 30%
	// delta = -0.3
	// newTarget = 0.2
	// diff = -0.3
	// need = 30
	// maxDrain = 100 * 0.1 = 10
	// Should return 10
	count = ctrl.calculatePoolDrainCount("p1", inference.ActionDecrease30)
	if count != 10 {
		t.Errorf("ActionDecrease30 (limited): expected 10, got %d", count)
	}
}

// MockPrometheusAPI for controller tests
type MockPrometheusAPI struct {
	v1.API
}

func (m *MockPrometheusAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	// Return empty vector for any query to simulate no metrics but successful connection
	return model.Vector{}, nil, nil
}

func TestStartStop(t *testing.T) {
	// Manual setup
	k8sClient := k8sfake.NewSimpleClientset()
	logger := slog.Default()
	inf := &inference.InferenceEngine{}

	// Use mock API for Prometheus
	prom, _ := metrics.NewClient(metrics.ClientConfig{
		API:    &MockPrometheusAPI{},
		Logger: logger,
	})

	cloud := &MockCloudProvider{DryRun: true}
	price := &MockPriceProvider{}

	cfg := Config{
		Cloud:               cloud,
		PriceProvider:       price,
		K8sClient:           k8sClient,
		Inference:           inf,
		PrometheusClient:    prom,
		Logger:              logger,
		RiskThreshold:       0.5,
		MaxDrainRatio:       0.1,
		ReconcileInterval:   10 * time.Second,
		ConfidenceThreshold: 0.5,
	}

	// We no longer need synthetic mode env vars because we provided a real (mocked) client
	// t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Start controller in a goroutine
	done := make(chan struct{})
	go func() {
		if err := c.Start(context.Background()); err != nil {
			t.Logf("Start returned: %v", err)
		}
		close(done)
	}()

	// Allow it to start
	time.Sleep(50 * time.Millisecond)

	// Stop it
	c.Stop()

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Error("Start did not return after Stop called")
	}
}

func TestController_ExecuteAction(t *testing.T) {
	k8sClient := k8sfake.NewSimpleClientset()
	logger := slog.Default()

	ctrl := &Controller{
		k8s:              k8sClient,
		logger:           logger,
		targetSpotRatio:  make(map[string]float64),
		currentSpotRatio: make(map[string]float64),
		// No drainer configured -> logs and returns nil
	}

	// Unmanaged node
	createNode(k8sClient, "node-unmanaged", "spot", "us-east-1a", "m5.large")
	unmanagedNode, _ := k8sClient.CoreV1().Nodes().Get(context.Background(), "node-unmanaged", metav1.GetOptions{})
	delete(unmanagedNode.Labels, "spotvortex.io/managed")
	k8sClient.CoreV1().Nodes().Update(context.Background(), unmanagedNode, metav1.UpdateOptions{})

	// Managed node
	createNode(k8sClient, "node-managed", "spot", "us-east-1a", "m5.large")

	// Test 1: Unmanaged node (should skip)
	err := ctrl.executeAction(context.Background(), NodeAssessment{NodeID: "node-unmanaged", Action: inference.ActionDecrease10})
	if err != nil {
		t.Errorf("unexpected error for unmanaged node: %v", err)
	}

	// Test 2: Managed node, Action Hold (should skip)
	err = ctrl.executeAction(context.Background(), NodeAssessment{NodeID: "node-managed", Action: inference.ActionHold})
	if err != nil {
		t.Errorf("unexpected error for HOLD: %v", err)
	}

	// Test 3: Managed Spot node, Action Decrease (should proceed to drain logic)
	// We didn't set 'drain', so it returns early with log "no drainer configured"
	err = ctrl.executeAction(context.Background(), NodeAssessment{NodeID: "node-managed", Action: inference.ActionDecrease10})
	if err != nil {
		t.Errorf("unexpected error for valid drain action (no drainer): %v", err)
	}
}

// TestBatchSteerKarpenterWeights covers the batch steering logic
func TestBatchSteerKarpenterWeights(t *testing.T) {
	k8sClient := k8sfake.NewSimpleClientset()
	dynClient := fake.NewSimpleDynamicClient(
		runtime.NewScheme(),
		makeTestNodePool("general-spot", 50),
		makeTestNodePool("general-od", 50),
	)
	logger := slog.Default()

	mgr := karpenter.NewNodePoolManager(dynClient, logger)
	ctrl := &Controller{
		k8s:           k8sClient,
		dynamicClient: dynClient,
		logger:        logger,
		karpenterCfg: config.KarpenterConfig{
			Enabled:                     true,
			SpotNodePoolSuffix:          "-spot",
			OnDemandNodePoolSuffix:      "-od",
			SpotWeight:                  80,
			OnDemandWeight:              20,
			WeightChangeCooldownSeconds: 1,
		},
		nodePoolMgr:      mgr,
		lastWeightChange: make(map[string]time.Time),
	}

	// Create a node with workload pool label. All actions below route through this node.
	createNode(k8sClient, "n1", "spot", "us-east-1a", "m5.large")
	node, err := k8sClient.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get test node: %v", err)
	}
	node.Labels[collector.WorkloadPoolLabel] = "general"
	if _, err := k8sClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to label test node: %v", err)
	}

	cases := []struct {
		name        string
		assessments []NodeAssessment
		wantSpot    int32
		wantOD      int32
	}{
		{
			name: "increase actions favor spot",
			assessments: []NodeAssessment{
				{NodeID: "n1", Action: inference.ActionIncrease10},
				{NodeID: "n1", Action: inference.ActionIncrease30},
			},
			wantSpot: 80,
			wantOD:   20,
		},
		{
			name: "decrease actions favor on-demand",
			assessments: []NodeAssessment{
				{NodeID: "n1", Action: inference.ActionDecrease10},
				{NodeID: "n1", Action: inference.ActionDecrease30},
			},
			wantSpot: 20,
			wantOD:   80,
		},
		{
			name: "emergency action favors on-demand",
			assessments: []NodeAssessment{
				{NodeID: "n1", Action: inference.ActionEmergencyExit},
			},
			wantSpot: 20,
			wantOD:   80,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl.historyLock.Lock()
			ctrl.lastWeightChange["general"] = time.Now().Add(-2 * time.Minute)
			ctrl.historyLock.Unlock()

			dynClient.ClearActions()
			ctrl.batchSteerKarpenterWeights(context.Background(), tc.assessments)

			actions := dynClient.Actions()
			if len(actions) == 0 {
				t.Fatal("expected nodepool patch actions, got none")
			}

			foundPatch := false
			for _, action := range actions {
				if action.GetVerb() == "patch" && action.GetResource().Resource == "nodepools" {
					foundPatch = true
					break
				}
			}
			if !foundPatch {
				t.Fatal("expected at least one patch action on nodepools")
			}

			gotSpot, err := mgr.GetWeight(context.Background(), "general-spot")
			if err != nil {
				t.Fatalf("failed to read general-spot weight: %v", err)
			}
			gotOD, err := mgr.GetWeight(context.Background(), "general-od")
			if err != nil {
				t.Fatalf("failed to read general-od weight: %v", err)
			}
			if gotSpot != tc.wantSpot || gotOD != tc.wantOD {
				t.Fatalf("weights mismatch: spot=%d od=%d want spot=%d od=%d", gotSpot, gotOD, tc.wantSpot, tc.wantOD)
			}
		})
	}
}

func makeTestNodePool(name string, weight int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"weight": weight,
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"requirements": []interface{}{
							map[string]interface{}{
								"key":      "karpenter.sh/capacity-type",
								"operator": "In",
								"values":   []interface{}{"spot", "on-demand"},
							},
						},
					},
				},
			},
		},
	}
}
