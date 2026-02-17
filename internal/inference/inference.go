// Package inference provides ONNX runtime integration for TFT model inference.
// This allows running the Python-trained model directly in Go for low-latency predictions.
package inference

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// PredictionInput contains the features for a single prediction.
type PredictionInput struct {
	NodeID       string
	Zone         string
	InstanceType string
	SpotPrice    float32
	CPUUsage     float32
	MemoryUsage  float32

	// Time features
	Hour      int
	DayOfWeek int
	IsWeekend bool
}

// PredictionOutput contains the model's prediction result.
type PredictionOutput struct {
	NodeID                  string
	InterruptionProbability float32
	Confidence              float32

	// Recommended action based on probability and confidence
	// Prime Directive: if confidence is low, recommend fallback
	RecommendedAction string
}

// ONNXSession defines the interface for an ONNX runtime session.
type ONNXSession interface {
	Run(inputs []ort.ArbitraryTensor, outputs []ort.ArbitraryTensor) error
	Destroy() error
}

// ONNXInference wraps the ONNX runtime for TFT model inference.
type ONNXInference struct {
	mu        sync.RWMutex
	session   ONNXSession
	modelPath string
	logger    *slog.Logger

	// Thresholds
	riskThreshold       float32
	confidenceThreshold float32
}

// InferenceConfig configures the ONNX inference engine.
type InferenceConfig struct {
	ModelPath           string
	RiskThreshold       float32 // Required, typically 0.85
	ConfidenceThreshold float32 // Required, typically 0.50
	Logger              *slog.Logger
	// Session allows injecting a mock session for testing.
	Session ONNXSession
}

// NewONNXInference creates a new ONNX inference engine.
func NewONNXInference(cfg InferenceConfig) (*ONNXInference, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	riskThreshold := cfg.RiskThreshold
	if riskThreshold == 0 {
		riskThreshold = 0.85
	}

	confidenceThreshold := cfg.ConfidenceThreshold
	if confidenceThreshold == 0 {
		confidenceThreshold = 0.50
	}

	inf := &ONNXInference{
		modelPath:           cfg.ModelPath,
		logger:              logger,
		riskThreshold:       riskThreshold,
		confidenceThreshold: confidenceThreshold,
		session:             cfg.Session,
	}

	// If a session was injected (testing), skip ORT initialization and loading
	if cfg.Session != nil {
		return inf, nil
	}

	// Initialize ONNX runtime
	SetSharedLibraryPath()
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("failed to initialize ONNX runtime: %w", err)
	}

	// Load model
	if err := inf.loadModel(); err != nil {
		return nil, err
	}

	return inf, nil
}

// loadModel loads the ONNX model from disk.
func (inf *ONNXInference) loadModel() error {
	inf.mu.Lock()
	defer inf.mu.Unlock()

	inf.logger.Info("loading ONNX model", "path", inf.modelPath)

	// Define input/output names
	// These MUST match the names used in brain/model.py
	inputNames := []string{"input"}
	outputNames := []string{"output"}

	session, err := ort.NewDynamicAdvancedSession(
		inf.modelPath,
		inputNames,
		outputNames,
		nil, // Use default session options
	)
	if err != nil {
		return fmt.Errorf("failed to create ONNX session: %w", err)
	}

	inf.session = session
	inf.logger.Info("ONNX model loaded successfully")
	return nil
}

// Predict runs inference on a single input.
// PRODUCTION MODE: Real ONNX inference only - no mocks or fallbacks.
func (inf *ONNXInference) Predict(ctx context.Context, input PredictionInput) (*PredictionOutput, error) {
	logger := inf.logger
	if logger == nil {
		logger = slog.Default()
		inf.logger = logger
	}

	logger.Debug("running prediction",
		"node_id", input.NodeID,
		"zone", input.Zone,
	)

	inf.mu.RLock()
	defer inf.mu.RUnlock()

	if inf.session == nil {
		return nil, fmt.Errorf("model not loaded")
	}

	// Prepare input tensor
	features := []float32{
		input.SpotPrice,
		input.CPUUsage,
		input.MemoryUsage,
		float32(input.Hour),
		float32(input.DayOfWeek),
		boolToFloat32(input.IsWeekend),
	}

	inputTensor, err := ort.NewTensor(ort.NewShape(1, int64(len(features))), features)
	if err != nil {
		return nil, fmt.Errorf("failed to create input tensor: %w", err)
	}
	defer inputTensor.Destroy()

	// Prepare output tensors
	probOutput := make([]float32, 1)
	confOutput := make([]float32, 1)

	probTensor, err := ort.NewTensor(ort.NewShape(1), probOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to create output tensor: %w", err)
	}
	defer probTensor.Destroy()

	confTensor, err := ort.NewTensor(ort.NewShape(1), confOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to create output tensor: %w", err)
	}
	defer confTensor.Destroy()

	// Run inference
	err = inf.session.Run(
		[]ort.ArbitraryTensor{inputTensor},
		[]ort.ArbitraryTensor{probTensor, confTensor},
	)
	if err != nil {
		return nil, fmt.Errorf("inference failed: %w", err)
	}

	probability := probOutput[0]
	confidence := confOutput[0]

	return &PredictionOutput{
		NodeID:                  input.NodeID,
		InterruptionProbability: probability,
		Confidence:              confidence,
		RecommendedAction:       inf.determineAction(probability, confidence),
	}, nil
}

// determineAction determines the recommended action based on probability and confidence.
// Prime Directive: if confidence is low, fall back to On-Demand.
func (inf *ONNXInference) determineAction(probability, confidence float32) string {
	// Prime Directive check: low confidence → safe mode
	if confidence < inf.confidenceThreshold {
		return "fallback_on_demand"
	}

	if probability > inf.riskThreshold {
		return "immediate_drain"
	} else if probability > 0.50 {
		return "drain"
	}
	return "hold"
}

// PredictBatch runs inference on multiple inputs.
func (inf *ONNXInference) PredictBatch(ctx context.Context, inputs []PredictionInput) ([]*PredictionOutput, error) {
	results := make([]*PredictionOutput, len(inputs))
	for i, input := range inputs {
		result, err := inf.Predict(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("prediction failed for node %s: %w", input.NodeID, err)
		}
		results[i] = result
	}
	return results, nil
}

// Close cleans up the ONNX runtime resources.
func (inf *ONNXInference) Close() error {
	inf.mu.Lock()
	defer inf.mu.Unlock()

	if inf.session != nil {
		if err := inf.session.Destroy(); err != nil {
			return err
		}
		inf.session = nil
	}

	return ort.DestroyEnvironment()
}

func SetSharedLibraryPath() {
	paths := []string{}
	if env := os.Getenv("ORT_SHARED_LIBRARY_PATH"); env != "" {
		paths = append(paths, env)
	}
	if env := os.Getenv("SPOTVORTEX_ONNXRUNTIME_PATH"); env != "" {
		paths = append(paths, env)
	}
	paths = appendVenvONNXRuntimeCandidates(paths)
	paths = append(paths, []string{
		"/opt/homebrew/lib/libonnxruntime.dylib",
		"/usr/local/lib/libonnxruntime.dylib",
		"/usr/lib/libonnxruntime.so",
	}...)
	seen := map[string]struct{}{}
	for _, p := range paths {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if _, err := os.Stat(p); err == nil {
			ort.SetSharedLibraryPath(p)
			return
		}
	}
	ort.SetSharedLibraryPath("onnxruntime")
}

func appendVenvONNXRuntimeCandidates(paths []string) []string {
	basePatterns := []string{
		".venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		".venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"vortex/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"vortex/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"spot-vortex/tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"spot-vortex/tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
	}
	prefixes := []string{
		".",
		"..",
		"../..",
		"../../..",
		"../../../..",
	}
	for _, prefix := range prefixes {
		for _, base := range basePatterns {
			pattern := filepath.Join(prefix, base)
			matches, err := filepath.Glob(pattern)
			if err != nil {
				continue
			}
			paths = append(paths, matches...)
		}
	}
	return paths
}

func boolToFloat32(b bool) float32 {
	if b {
		return 1.0
	}
	return 0.0
}
