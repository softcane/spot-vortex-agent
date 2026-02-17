// Package inference provides ONNX runtime integration tests.
// These tests require real ONNX models - NO mocks.
package inference

import (
	"context"
	"os"
	"testing"
)

// TestONNXInference_RealModel tests inference with a real ONNX model.
// REQUIRES: ONNX_MODEL_PATH environment variable set to valid model.
func TestONNXInference_RealModel(t *testing.T) {
	modelPath := os.Getenv("ONNX_MODEL_PATH")
	if modelPath == "" {
		t.Skip("ONNX_MODEL_PATH not set - skipping real model test")
	}

	inf, err := NewONNXInference(InferenceConfig{
		ModelPath:           modelPath,
		RiskThreshold:       0.85,
		ConfidenceThreshold: 0.50,
	})
	if err != nil {
		t.Fatalf("failed to create inference engine: %v", err)
	}
	defer inf.Close()

	// Test with realistic input
	input := PredictionInput{
		NodeID:       "test-node-001",
		Zone:         "us-east-1a",
		InstanceType: "m5.4xlarge",
		SpotPrice:    0.042,
		CPUUsage:     0.73,
		MemoryUsage:  0.65,
		Hour:         14,
		DayOfWeek:    2,
		IsWeekend:    false,
	}

	output, err := inf.Predict(context.Background(), input)
	if err != nil {
		t.Fatalf("prediction failed: %v", err)
	}

	// Validate output ranges
	if output.InterruptionProbability < 0 || output.InterruptionProbability > 1 {
		t.Errorf("probability out of range: %v", output.InterruptionProbability)
	}

	if output.Confidence < 0 || output.Confidence > 1 {
		t.Errorf("confidence out of range: %v", output.Confidence)
	}

	if output.NodeID != input.NodeID {
		t.Errorf("node ID mismatch: got %s, want %s", output.NodeID, input.NodeID)
	}

	validActions := map[string]bool{
		"hold":               true,
		"drain":              true,
		"immediate_drain":    true,
		"fallback_on_demand": true,
	}
	if !validActions[output.RecommendedAction] {
		t.Errorf("invalid recommended action: %s", output.RecommendedAction)
	}

	t.Logf("Prediction result: prob=%.4f, conf=%.4f, action=%s",
		output.InterruptionProbability,
		output.Confidence,
		output.RecommendedAction,
	)
}

// TestONNXInference_BatchPrediction tests batch inference with real model.
func TestONNXInference_BatchPrediction(t *testing.T) {
	modelPath := os.Getenv("ONNX_MODEL_PATH")
	if modelPath == "" {
		t.Skip("ONNX_MODEL_PATH not set - skipping real model test")
	}

	inf, err := NewONNXInference(InferenceConfig{
		ModelPath:           modelPath,
		RiskThreshold:       0.85,
		ConfidenceThreshold: 0.50,
	})
	if err != nil {
		t.Fatalf("failed to create inference engine: %v", err)
	}
	defer inf.Close()

	// Test batch of inputs representing different node scenarios
	inputs := []PredictionInput{
		{NodeID: "node-1", Zone: "us-east-1a", SpotPrice: 0.042, CPUUsage: 0.73},
		{NodeID: "node-2", Zone: "us-east-1b", SpotPrice: 0.038, CPUUsage: 0.45},
		{NodeID: "node-3", Zone: "us-east-1c", SpotPrice: 0.085, CPUUsage: 0.91}, // High price/usage
	}

	outputs, err := inf.PredictBatch(context.Background(), inputs)
	if err != nil {
		t.Fatalf("batch prediction failed: %v", err)
	}

	if len(outputs) != len(inputs) {
		t.Fatalf("output count mismatch: got %d, want %d", len(outputs), len(inputs))
	}

	for i, out := range outputs {
		if out.NodeID != inputs[i].NodeID {
			t.Errorf("node %d: ID mismatch", i)
		}
		t.Logf("Node %s: prob=%.4f, conf=%.4f, action=%s",
			out.NodeID, out.InterruptionProbability, out.Confidence, out.RecommendedAction)
	}
}

// TestONNXInference_ModelNotLoaded verifies error handling when model isn't loaded.
func TestONNXInference_ModelNotLoaded(t *testing.T) {
	// Create inference without loading model (simulate failure case)
	inf := &ONNXInference{
		riskThreshold:       0.85,
		confidenceThreshold: 0.50,
	}

	_, err := inf.Predict(context.Background(), PredictionInput{NodeID: "test"})
	if err == nil {
		t.Error("expected error when model not loaded")
	}

	expectedMsg := "model not loaded"
	if err.Error() != expectedMsg {
		t.Errorf("unexpected error message: got %q, want %q", err.Error(), expectedMsg)
	}
}
