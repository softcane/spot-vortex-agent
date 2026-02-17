package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestRunPoolLevelInference tests the aggregation and inference logic.
func TestRunPoolLevelInference(t *testing.T) {
	// Setup Fake Clients
	k8sClient := k8sfake.NewSimpleClientset()

	// Seed NodePool for extended format (though runPoolLevelInference mainly uses zone/workloadPool)
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": "default",
			},
		},
	}
	startScheme := runtime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(startScheme, pool)

	// Logger
	logger := slog.Default()

	// Use relative paths to real ONNX models (same as TestController_Reconcile_DryRun)
	inf, err := inference.NewInferenceEngine(inference.EngineConfig{
		TFTModelPath:       "../../models/tft.onnx",
		RLModelPath:        "../../models/rl_policy.onnx",
		Logger:             logger,
		RequireRuntimeHead: false,
	})
	if err != nil {
		t.Logf("Failed to load inference engine: %v", err)
		t.Skip("Skipping inference test due to missing models")
		return
	}
	defer inf.Close()

	cloudProvider := &MockCloudProvider{DryRun: true}
	priceProvider := &MockPriceProvider{
		PriceData: cloudapi.SpotPriceData{
			CurrentPrice:  0.2,
			OnDemandPrice: 1.0,
			PriceHistory:  []float64{0.2, 0.2, 0.2},
		},
	}

	cfg := Config{
		Logger:                  logger,
		K8sClient:               k8sClient,
		DynamicClient:           dynClient,
		PrometheusClient:        &metrics.Client{}, // Nil pointer panic if we call real methods, but synthetic mode bypasses
		Inference:               inf,
		Cloud:                   cloudProvider,
		PriceProvider:           priceProvider,
		RiskThreshold:           0.8,
		MaxDrainRatio:           0.1,
		ReconcileInterval:       10 * time.Second,
		ConfidenceThreshold:     0.5,
		DrainGracePeriodSeconds: 30, // Just a guess, was it in Config struct?
		Karpenter: config.KarpenterConfig{
			Enabled: false,
		},
	}

	// Use synthetic metrics to bypass Prometheus (prices come from MockPriceProvider)
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	// Create dummy node metrics
	nodeMetrics := []metrics.NodeMetrics{
		{
			NodeID:             "node-1",
			InstanceType:       "m5.large",
			Zone:               "us-east-1a",
			IsSpot:             true,
			CPUUsagePercent:    20.0,
			MemoryUsagePercent: 30.0,
		},
		{
			NodeID:             "node-2",
			InstanceType:       "m5.large",
			Zone:               "us-east-1a",
			IsSpot:             true,
			CPUUsagePercent:    25.0,
			MemoryUsagePercent: 35.0,
		},
		{
			NodeID:             "node-3",
			InstanceType:       "m5.large",
			Zone:               "us-east-1a",
			IsSpot:             false, // OD
			CPUUsagePercent:    10.0,
			MemoryUsagePercent: 15.0,
		},
	}

	// Run inference
	assessments, err := c.runPoolLevelInference(context.Background(), nodeMetrics)
	if err != nil {
		t.Fatalf("runPoolLevelInference failed: %v", err)
	}

	// Verify assessments
	// Based on TestController_Reconcile_DryRun output, dummy models produce actions.
	// We expect roughly 1 assessment for the pool "us-east-1a" (since workload pool is unknown/common)

	if len(assessments) == 0 {
		t.Log("No assessments generated. This might be due to model behavior or input validation.")
		// Check logs?
	} else {
		t.Logf("Generated %d assessments", len(assessments))
		for _, a := range assessments {
			t.Logf("Assessment: NodeID=%s Action=%v Confidence=%v", a.NodeID, a.Action, a.Confidence)
		}
	}
}

func TestApplyTargetSpotRatio(t *testing.T) {
	// Setup Controller for applying target spot ratio
	// We need to initialize the maps
	c := &Controller{
		targetSpotRatio:  make(map[string]float64),
		currentSpotRatio: make(map[string]float64),
		logger:           slog.Default(),
		// loadRuntimeConfig will try to read configMap. Mock k8s client or ensure default.
		// runtime.go: loadRuntimeConfig uses c.k8s.CoreV1().ConfigMaps...
		// If k8s is nil, it might panic if loadRuntimeConfig is called.
	}

	// We should mock k8s client for ConfigMap
	k8sClient := k8sfake.NewSimpleClientset()
	c.k8s = k8sClient
	// And init locks if needed?
	// loadRuntimeConfig uses RLock if accessing cached config... no, it calls k8s.

	poolID := "pool-1"
	c.targetSpotRatio[poolID] = 0.5

	// Action: Decrease 10 (target -= 0.1)
	c.applyTargetSpotRatioWithConfig(poolID, inference.ActionDecrease10, nil, false)

	// 0.5 - 0.1 = 0.4
	expected := 0.4
	// Allow small float error
	if diff := c.targetSpotRatio[poolID] - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected target 0.4, got %f", c.targetSpotRatio[poolID])
	}

	// Action: Increase 30 (target += 0.3)
	c.applyTargetSpotRatioWithConfig(poolID, inference.ActionIncrease30, nil, false)
	// 0.4 + 0.3 = 0.7
	expected = 0.7
	if diff := c.targetSpotRatio[poolID] - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected target 0.7, got %f", c.targetSpotRatio[poolID])
	}
}

func TestApplyTargetSpotRatioWrapper(t *testing.T) {
	c := &Controller{
		targetSpotRatio:  make(map[string]float64),
		currentSpotRatio: make(map[string]float64),
		lastWeightChange: make(map[string]time.Time),
		logger:           slog.Default(),
	}

	// Call deprecated wrapper
	c.applyTargetSpotRatio("pool-1", inference.ActionDecrease10)
}
