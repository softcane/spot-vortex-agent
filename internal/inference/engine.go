package inference

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

// Action represents the decision from the RL policy.
type Action int

const (
	ActionHold          Action = 0
	ActionDecrease10    Action = 1
	ActionDecrease30    Action = 2
	ActionIncrease10    Action = 3
	ActionIncrease30    Action = 4
	ActionEmergencyExit Action = 5
)

// ActionToString maps actions to human-readable strings.
func ActionToString(a Action) string {
	switch a {
	case ActionHold:
		return "HOLD"
	case ActionDecrease10:
		return "DECREASE_10"
	case ActionDecrease30:
		return "DECREASE_30"
	case ActionIncrease10:
		return "INCREASE_10"
	case ActionIncrease30:
		return "INCREASE_30"
	case ActionEmergencyExit:
		return "EMERGENCY_EXIT"
	default:
		return "UNKNOWN"
	}
}

// InferenceEngine coordinates the TFT and RL models.
type InferenceEngine struct {
	mu     sync.RWMutex
	logger *slog.Logger

	tftModel *Model // High-Fidelity Distilled TFT
	rlModel  *Model // DQN V6 Champion
	pysr     *PySREngine
	scope    *ModelContract

	builder *FeatureBuilder
}

// RLFallbackError means TFT scores were computed but RL action selection failed.
// Deterministic-active mode can use the embedded scores and continue safely.
type RLFallbackError struct {
	CapacityScore float32
	RuntimeScore  float32
	Cause         error
}

func (e *RLFallbackError) Error() string {
	if e == nil || e.Cause == nil {
		return "rl inference failed after TFT scores were computed"
	}
	return e.Cause.Error()
}

func (e *RLFallbackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// AsRLFallbackError extracts an RL fallback error and its TFT scores.
func AsRLFallbackError(err error) (*RLFallbackError, bool) {
	var target *RLFallbackError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// EngineConfig configures the InferenceEngine.
type EngineConfig struct {
	TFTModelPath         string
	RLModelPath          string
	PySRCalibrationPath  string // Optional: defaults to models/pysr/calibration_equation.txt
	PySRFusionPath       string // Optional: defaults to models/pysr/context_equation.txt
	ModelManifestPath    string
	ExpectedCloud        string
	RequireModelContract bool
	Logger               *slog.Logger
}

// NewInferenceEngine creates a new inference engine.
func NewInferenceEngine(cfg EngineConfig) (*InferenceEngine, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	manifestPath := strings.TrimSpace(cfg.ModelManifestPath)
	if manifestPath == "" {
		manifestPath = defaultManifestPath(cfg.TFTModelPath)
	}

	scope, err := LoadModelContract(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load model contract from %s: %w", manifestPath, err)
	}
	if cfg.RequireModelContract {
		if scope == nil {
			return nil, fmt.Errorf(
				"model contract is required but missing: %s",
				manifestPath,
			)
		}
		if err := VerifyManifestArtifacts(manifestPath, cfg.TFTModelPath, cfg.RLModelPath); err != nil {
			return nil, fmt.Errorf("manifest artifact verification failed: %w", err)
		}
	}
	if scope != nil {
		expectedCloud := strings.TrimSpace(strings.ToLower(cfg.ExpectedCloud))
		if expectedCloud == "" {
			expectedCloud = strings.TrimSpace(strings.ToLower(os.Getenv("SPOTVORTEX_CLOUD")))
		}
		if expectedCloud != "" && scope.Cloud != "" && scope.Cloud != expectedCloud {
			return nil, fmt.Errorf(
				"model cloud mismatch: expected=%s manifest=%s path=%s",
				expectedCloud,
				scope.Cloud,
				manifestPath,
			)
		}
	}

	// Initialize ONNX runtime
	SetSharedLibraryPath()
	if err := ort.InitializeEnvironment(); err != nil {
		logger.Warn("ONNX Runtime already initialized or failed", "error", err)
	}

	// Load models using the shipped dual-head TFT contract.
	tft, err := NewModel(cfg.TFTModelPath, []string{"input"}, []string{"capacity_score", "runtime_score"})
	if err != nil {
		return nil, fmt.Errorf("failed to load TFT model with outputs capacity_score,runtime_score: %w", err)
	}

	rl, err := NewModel(cfg.RLModelPath, []string{"state"}, []string{"q_values"})
	if err != nil {
		return nil, fmt.Errorf("failed to load RL model: %w", err)
	}

	pysrCalibPath := cfg.PySRCalibrationPath
	if pysrCalibPath == "" {
		pysrCalibPath = "models/pysr/calibration_equation.txt"
	}
	pysrFusionPath := cfg.PySRFusionPath
	if pysrFusionPath == "" {
		pysrFusionPath = "models/pysr/context_equation.txt"
	}

	pysrEngine := NewPySREngine(
		logger,
		pysrCalibPath,
		pysrFusionPath,
	)

	if scope != nil {
		logger.Info("model contract loaded",
			"manifest", manifestPath,
			"cloud", scope.Cloud,
			"supported_families", len(scope.SupportedInstanceFamilies),
			"artifacts", len(scope.ArtifactChecksums),
		)
	} else {
		logger.Warn("model contract missing; unsupported-family enforcement is disabled",
			"manifest", manifestPath,
			"hint", "provide MODEL_MANIFEST.json or SPOTVORTEX_SUPPORTED_INSTANCE_FAMILIES",
		)
	}

	engine := &InferenceEngine{
		logger:   logger,
		tftModel: tft,
		rlModel:  rl,
		pysr:     pysrEngine,
		scope:    scope,
		builder:  NewFeatureBuilder(),
	}

	if err := engine.validateModelContracts(); err != nil {
		engine.Close()
		return nil, err
	}

	return engine, nil
}

// Predict calculates the optimal action for a node.
// This implements the full V2 pipeline: NodeState -> TFT -> PySR -> RiskScaling -> RL -> Action.
func (e *InferenceEngine) Predict(ctx context.Context, nodeID string, state NodeState, riskMultiplier float64) (Action, float32, float32, error) {
	action, capacity, _, confidence, err := e.PredictDetailed(ctx, nodeID, state, riskMultiplier)
	return action, capacity, confidence, err
}

// PredictDetailed returns action plus both TFT heads used by RL (capacity/runtime).
func (e *InferenceEngine) PredictDetailed(ctx context.Context, nodeID string, state NodeState, riskMultiplier float64) (Action, float32, float32, float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. Build TFT Input (TFTHistorySteps x TFTFeatureCount features)
	// Note: We use the builder to maintain price history
	e.builder.UpdatePriceHistory(nodeID, state.SpotPrice)
	tftFeatures := e.builder.BuildTFTInput(nodeID, state)

	tftInputShape := ort.NewShape(1, TFTHistorySteps, TFTFeatureCount)
	tftInputTensor, err := ort.NewTensor(tftInputShape, tftFeatures)
	if err != nil {
		return ActionHold, 0, 0, 0, fmt.Errorf("failed to create TFT tensor: %w", err)
	}
	defer tftInputTensor.Destroy()

	// 2. Run TFT for Capacity Score (Risk Tracking)
	tftInputs := map[string]*ort.Tensor[float32]{"input": tftInputTensor}
	tftOutputs, err := e.tftModel.Predict(tftInputs)
	if err != nil {
		return ActionHold, 0, 0, 0, fmt.Errorf("TFT inference failed: %w", err)
	}
	defer destroyTensorMap(tftOutputs)

	rawScore, rawRuntimeScore, err := extractRiskScores(tftOutputs)
	if err != nil {
		return ActionHold, 0, 0, 0, err
	}

	// 2.5 Apply PySR Symbolic Regression (Gap 3)
	calibrationScore := float64(rawScore)
	priceVolatility := 0.0
	if len(state.PriceHistory) > 1 {
		priceVolatility = calculateStdDev(state.PriceHistory)
	}
	if e.pysr != nil {
		if score, ok := e.pysr.ApplyCalibration(map[string]float64{
			"capacity_score":   float64(rawScore),
			"price_volatility": priceVolatility,
		}); ok {
			calibrationScore = score
		}
	}

	finalScore := calibrationScore
	if e.pysr != nil {
		if score, ok := e.pysr.ApplyFusion(map[string]float64{
			"pysr_calibrated_risk": calibrationScore,
			"pod_startup_time":     state.PodStartupTime,
			"outage_penalty_hours": state.OutagePenaltyHours,
			"cluster_utilization":  state.ClusterUtilization,
			"priority_score":       state.PriorityScore,
		}); ok {
			finalScore = score
		}
	}

	// 2.6 Apply Runtime Risk Multiplier (Gap 4)
	if riskMultiplier != 1.0 {
		// Clamp to avoid div/0 or log errors
		if finalScore < 1e-6 {
			finalScore = 1e-6
		} else if finalScore > 1.0-1e-6 {
			finalScore = 1.0 - 1e-6
		}
		odds := finalScore / (1.0 - finalScore)
		adjustedOdds := math.Pow(odds, riskMultiplier)
		finalScore = adjustedOdds / (1.0 + adjustedOdds)
	}

	runtimeScore := float64(rawRuntimeScore)
	if riskMultiplier != 1.0 {
		if runtimeScore < 1e-6 {
			runtimeScore = 1e-6
		} else if runtimeScore > 1.0-1e-6 {
			runtimeScore = 1.0 - 1e-6
		}
		odds := runtimeScore / (1.0 - runtimeScore)
		adjustedOdds := math.Pow(odds, riskMultiplier)
		runtimeScore = adjustedOdds / (1.0 + adjustedOdds)
	}

	// Ensure RL input reflects runtime risk from the TFT runtime head.
	state.RuntimeScore = runtimeScore

	// 3. Build RL Input (13 features)
	rlFeatures := e.builder.BuildRLInput(state, finalScore)
	wrapRLFallback := func(cause error) error {
		return &RLFallbackError{
			CapacityScore: float32(finalScore),
			RuntimeScore:  float32(runtimeScore),
			Cause:         cause,
		}
	}

	rlInputShape := ort.NewShape(1, int64(len(rlFeatures)))
	rlInputTensor, err := ort.NewTensor(rlInputShape, rlFeatures)
	if err != nil {
		return ActionHold, float32(finalScore), float32(runtimeScore), 0, wrapRLFallback(fmt.Errorf("failed to create RL tensor: %w", err))
	}
	defer rlInputTensor.Destroy()

	// 4. Run RL for Action selection
	rlInputs := map[string]*ort.Tensor[float32]{"state": rlInputTensor}
	rlOutputs, err := e.rlModel.Predict(rlInputs)
	if err != nil {
		return ActionHold, float32(finalScore), float32(runtimeScore), 0, wrapRLFallback(fmt.Errorf("RL inference failed: %w", err))
	}
	defer destroyTensorMap(rlOutputs)

	// Q-values output [batch, 6] -> find argmax (V2 spec)
	qTensor, ok := rlOutputs["q_values"]
	if !ok || qTensor == nil {
		return ActionHold, float32(finalScore), float32(runtimeScore), 0, wrapRLFallback(fmt.Errorf("RL inference failed: missing q_values output"))
	}
	qValues := qTensor.GetData()
	if len(qValues) == 0 {
		return ActionHold, float32(finalScore), float32(runtimeScore), 0, wrapRLFallback(fmt.Errorf("RL inference failed: empty q_values output"))
	}

	bestAction := ActionHold
	maxQ := qValues[0]
	for i := 1; i < len(qValues); i++ {
		if qValues[i] > maxQ {
			maxQ = qValues[i]
			bestAction = Action(i)
		}
	}

	e.logger.Info("Inference complete",
		"node_id", nodeID,
		"capacity_score", finalScore,
		"raw_score", rawScore,
		"action", ActionToString(bestAction),
		"confidence", maxQ,
	)

	// In DQN, we'll normalize maxQ to a 0-1 confidence proxy if needed
	// For now, if maxQ > -100 (not total garbage), we call it confident
	confidence := float32(1.0)
	if maxQ < -1000 {
		confidence = 0.1
	}

	return bestAction, float32(finalScore), float32(runtimeScore), confidence, nil
}

func (e *InferenceEngine) validateModelContracts() error {
	seedHistory := []float64{0.31, 0.32, 0.30, 0.29, 0.31, 0.33, 0.34, 0.33, 0.32, 0.31, 0.30, 0.29}
	contractState := NodeState{
		SpotPrice:          0.31,
		OnDemandPrice:      0.97,
		PriceHistory:       seedHistory,
		CPUUsage:           0.50,
		MemoryUsage:        0.55,
		PodStartupTime:     30,
		MigrationCost:      1.0,
		ClusterUtilization: 0.60,
		OutagePenaltyHours: 1.0,
		TimeSinceMigration: 5,
		RuntimeScore:       0.15,
		IsSpot:             true,
		CurrentSpotRatio:   0.60,
		TargetSpotRatio:    0.60,
		Timestamp:          time.Unix(1700000000, 0).UTC(),
	}

	tftTensor, err := ort.NewTensor(
		ort.NewShape(1, TFTHistorySteps, TFTFeatureCount),
		e.builder.BuildTFTInput("__contract__", contractState),
	)
	if err != nil {
		return fmt.Errorf("failed to create TFT contract tensor: %w", err)
	}
	defer tftTensor.Destroy()

	tftOutputs, err := e.tftModel.Predict(map[string]*ort.Tensor[float32]{"input": tftTensor})
	if err != nil {
		return fmt.Errorf("TFT model contract check failed: %w", err)
	}
	defer destroyTensorMap(tftOutputs)

	if err := validateTFTOutputContract(tftOutputs); err != nil {
		return err
	}

	rlTensor, err := ort.NewTensor(
		ort.NewShape(1, RLFeatureCount),
		e.builder.BuildRLInput(contractState, 0.25),
	)
	if err != nil {
		return fmt.Errorf("failed to create RL contract tensor: %w", err)
	}
	defer rlTensor.Destroy()

	rlOutputs, err := e.rlModel.Predict(map[string]*ort.Tensor[float32]{"state": rlTensor})
	if err != nil {
		return fmt.Errorf("RL model contract check failed: %w", err)
	}
	defer destroyTensorMap(rlOutputs)

	if err := validateRLQValuesOutputContract(rlOutputs); err != nil {
		return err
	}

	return nil
}

func validateTFTOutputContract(tftOutputs map[string]*ort.Tensor[float32]) error {
	_, _, err := extractRiskScores(tftOutputs)
	if err != nil {
		return fmt.Errorf("TFT model contract check failed: %w", err)
	}
	return nil
}

func validateRLQValuesOutputContract(rlOutputs map[string]*ort.Tensor[float32]) error {
	qValuesTensor, ok := rlOutputs["q_values"]
	if !ok || qValuesTensor == nil {
		return fmt.Errorf("RL model contract check failed: missing q_values output")
	}

	qShape := qValuesTensor.GetShape()
	if len(qShape) == 0 || qShape[len(qShape)-1] != 6 {
		return fmt.Errorf("RL model contract check failed: expected q_values last dimension=6, got shape=%v", qShape)
	}
	if len(qValuesTensor.GetData()) < 6 {
		return fmt.Errorf("RL model contract check failed: q_values output contains fewer than 6 values")
	}
	return nil
}

func destroyTensorMap(tensors map[string]*ort.Tensor[float32]) {
	for _, tensor := range tensors {
		if tensor != nil {
			tensor.Destroy()
		}
	}
}

func extractRiskScores(outputs map[string]*ort.Tensor[float32]) (float32, float32, error) {
	capacityTensor, ok := outputs["capacity_score"]
	if !ok || capacityTensor == nil {
		return 0, 0, fmt.Errorf("TFT output missing: capacity_score")
	}

	runtimeTensor, ok := outputs["runtime_score"]
	if !ok || runtimeTensor == nil {
		return 0, 0, fmt.Errorf("TFT output missing: runtime_score")
	}

	capacity, err := extractRiskScoreValue(capacityTensor)
	if err != nil {
		return 0, 0, fmt.Errorf("capacity_score: %w", err)
	}

	runtime, err := extractRiskScoreValue(runtimeTensor)
	if err != nil {
		return 0, 0, fmt.Errorf("runtime_score: %w", err)
	}

	return capacity, runtime, nil
}

func extractRiskScoreValue(tensor *ort.Tensor[float32]) (float32, error) {
	if tensor == nil {
		return 0, fmt.Errorf("tensor is nil")
	}

	data := tensor.GetData()
	if len(data) == 0 {
		return 0, fmt.Errorf("tensor is empty")
	}

	shape := tensor.GetShape()
	if len(shape) == 3 && shape[1] > 0 && shape[2] > 0 {
		horizon := int(shape[1])
		quantiles := int(shape[2])
		leadSteps := 6 // 60m / 10m
		if leadSteps >= horizon {
			leadSteps = horizon - 1
		}
		offset := leadSteps*quantiles + quantiles/2
		if offset >= 0 && offset < len(data) {
			return data[offset], nil
		}
		return 0, fmt.Errorf("tensor shape %v produced invalid offset %d", shape, offset)
	}

	if len(shape) == 2 && shape[1] > 0 {
		width := int(shape[1])
		idx := width / 2
		if idx >= 0 && idx < len(data) {
			return data[idx], nil
		}
		return 0, fmt.Errorf("tensor shape %v produced invalid index %d", shape, idx)
	}

	return data[0], nil
}

// Close releases model resources.
func (e *InferenceEngine) Close() {
	if e.tftModel != nil {
		e.tftModel.Close()
	}
	if e.rlModel != nil {
		e.rlModel.Close()
	}
	ort.DestroyEnvironment()
}

// SupportsInstanceType checks model scope restrictions (if provided).
func (e *InferenceEngine) SupportsInstanceType(instanceType string) (bool, string) {
	if e == nil {
		return true, ""
	}
	return e.scope.SupportsInstanceType(instanceType)
}
