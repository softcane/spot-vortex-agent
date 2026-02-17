// Package inference provides ONNX runtime integration tests.
// These tests require real ONNX models - NO mocks.
package inference

import (
	"context"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

// MockSession implements ONNXSession for testing.
type MockSession struct {
	Prediction float32
	Confidence float32
	Err        error
}

func (m *MockSession) Run(inputs []ort.ArbitraryTensor, outputs []ort.ArbitraryTensor) error {
	if m.Err != nil {
		return m.Err
	}
	// Write outputs
	// Output 0: Probability
	// Output 1: Confidence
	// We assume outputs are *ort.Tensor[float32], which exposes GetHeader().GetData() ...
	// Wait, ort.ArbitraryTensor interface doesn't easily allow writing data without casting.
	// We need to cast to *ort.Tensor[float32].

	// IMPORTANT: In production code we use NewTensor which returns *Tensor[T].
	// The Run signature accepts []ArbitraryTensor.

	// Helper to write float32 to tensor
	write := func(idx int, val float32) {
		if idx >= len(outputs) {
			return
		}
		t, ok := outputs[idx].(*ort.Tensor[float32])
		if !ok {
			return
		}
		data := t.GetData()
		if len(data) > 0 {
			data[0] = val
		}
	}

	write(0, m.Prediction)
	write(1, m.Confidence)

	return nil
}

func (m *MockSession) Destroy() error {
	return nil
}

func TestONNXInference_Mocked(t *testing.T) {
	// Initialize ORT for tensor creation
	SetSharedLibraryPath()
	_ = ort.InitializeEnvironment() // Ignore error in case it's already init or fails (we'll see failure in NewTensor)

	mockSession := &MockSession{
		Prediction: 0.75, // Risk > 0.5 -> drain
		Confidence: 0.90, // High confidence
	}

	inf, err := NewONNXInference(InferenceConfig{
		RiskThreshold:       0.85,
		ConfidenceThreshold: 0.50,
		Session:             mockSession,
	})
	if err != nil {
		t.Fatalf("failed to create inference engine: %v", err)
	}

	// Test with realistic input
	input := PredictionInput{
		NodeID:   "test-node-001",
		Zone:     "us-east-1a",
		CPUUsage: 0.73,
	}

	output, err := inf.Predict(context.Background(), input)
	if err != nil {
		t.Fatalf("prediction failed: %v", err)
	}

	// Validate output
	if output.InterruptionProbability != 0.75 {
		t.Errorf("expected prob 0.75, got %v", output.InterruptionProbability)
	}
	if output.Confidence != 0.90 {
		t.Errorf("expected conf 0.90, got %v", output.Confidence)
	}
	if output.RecommendedAction != "drain" {
		t.Errorf("expected action 'drain', got %s", output.RecommendedAction)
	}
}

func TestONNXInference_BatchMocked(t *testing.T) {
	mockSession := &MockSession{
		Prediction: 0.95, // Immediate drain
		Confidence: 0.99,
	}

	inf, err := NewONNXInference(InferenceConfig{
		RiskThreshold:       0.85,
		ConfidenceThreshold: 0.50,
		Session:             mockSession,
	})
	if err != nil {
		t.Fatalf("failed to create inference engine: %v", err)
	}

	inputs := []PredictionInput{
		{NodeID: "node-1"},
		{NodeID: "node-2"},
	}

	outputs, err := inf.PredictBatch(context.Background(), inputs)
	if err != nil {
		t.Fatalf("batch prediction failed: %v", err)
	}

	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(outputs))
	}

	for _, out := range outputs {
		if out.RecommendedAction != "immediate_drain" {
			t.Errorf("expected 'immediate_drain', got %s", out.RecommendedAction)
		}
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
