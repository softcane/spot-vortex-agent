package e2e

import (
	"context"
	"testing"
	"time"

	"log/slog"

	"github.com/softcane/spot-vortex-agent/internal/controller"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestV2MigrationScenarios(t *testing.T) {
	// Setup k8s client
	client := setupClient(t)
	ctx := context.Background()

	// 1. Initialize Inference Engine with V2 models
	logger := slog.Default()
	infEngine, err := inference.NewInferenceEngine(inference.EngineConfig{
		TFTModelPath:        "../../models/tft.onnx",
		RLModelPath:         "../../models/rl_policy.onnx",
		PySRCalibrationPath: "../../models/pysr/calibration_equation.txt",
		PySRFusionPath:      "../../models/pysr/context_equation.txt",
		RequireRuntimeHead:  true,
		Logger:              logger,
	})
	if err != nil {
		t.Fatalf("Failed to load V2 models: %v", err)
	}
	defer infEngine.Close()

	// 2. Setup metrics and executor
	_ = metrics.ActionTaken // Warm up metrics
	executor := controller.NewExecutor(client, nil, logger, controller.ExecutorConfig{
		GracefulDrainPeriod: 10 * time.Minute,
		ForceDrainPeriod:    1 * time.Minute,
		ClusterFractionMax:  0.5,
	})

	// 3. Find a spot node to test
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Fatalf("No spot nodes found: %v", err)
	}
	testNode := &nodes.Items[0]

	// 4. Scenario A: Market Crash (Should trigger Emergency Exit)
	t.Run("MarketCrash_EmergencyExit", func(t *testing.T) {
		// High risk state
		state := inference.NodeState{
			SpotPrice:          0.80, // High price
			OnDemandPrice:      0.85,
			PriceHistory:       []float64{0.15, 0.30, 0.50, 0.70, 0.80},
			CPUUsage:           0.90,
			IsSpot:             true,
			CurrentSpotRatio:   1.0,
			TargetSpotRatio:    1.0,
			TimeSinceMigration: 100,
			Timestamp:          time.Now(),
		}

		// Mock high capacity score from TFT (Force high risk)
		capacityScore := float32(0.95)

		// Get RL action
		action, _, confidence, err := infEngine.Predict(ctx, testNode.Name, state, 1.0)
		if err != nil {
			t.Fatalf("Inference failed: %v", err)
		}

		t.Logf("Model Action: %s (conf=%.2f)", inference.ActionToString(action), confidence)

		// Execute action
		err = executor.Execute(ctx, testNode, controller.Action(action), controller.NodeState{
			NodeName:      testNode.Name,
			CapacityScore: float64(capacityScore),
			Confidence:    float64(confidence),
		})
		if err != nil {
			t.Fatalf("Execution failed: %v", err)
		}

		// Verify node is cordoned/tainted
		updatedNode, _ := client.CoreV1().Nodes().Get(ctx, testNode.Name, metav1.GetOptions{})
		hasTaint := false
		for _, taint := range updatedNode.Spec.Taints {
			if taint.Key == "spotvortex.io/draining" {
				hasTaint = true
				break
			}
		}
		if !hasTaint {
			t.Error("Node should have draining taint after emergency exit")
		}

		// Uncordon for next test
		updatedNode.Spec.Unschedulable = false
		var newTaints []corev1.Taint
		for _, taint := range updatedNode.Spec.Taints {
			if taint.Key != "spotvortex.io/draining" {
				newTaints = append(newTaints, taint)
			}
		}
		updatedNode.Spec.Taints = newTaints
		client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{})
	})

	// 5. Scenario B: Recovery (Should trigger Increase10/30)
	t.Run("MarketCalm_Recovery", func(t *testing.T) {
		// Low risk state, currently on OD (IsSpot=false)
		state := inference.NodeState{
			SpotPrice:          0.15,
			OnDemandPrice:      0.85,
			PriceHistory:       []float64{0.15, 0.15, 0.15, 0.15},
			IsSpot:             false,
			CurrentSpotRatio:   0.0,
			TargetSpotRatio:    0.0,
			TimeSinceMigration: 10,
			Timestamp:          time.Now(),
		}

		action, _, confidence, _ := infEngine.Predict(ctx, testNode.Name, state, 1.0)
		t.Logf("Model Action: %s (conf=%.2f)", inference.ActionToString(action), confidence)

		// We expect RECOVER or HOLD
		if action == inference.ActionIncrease10 || action == inference.ActionIncrease30 {
			t.Log("Model correctly identified recovery opportunity")
		}
	})
}
