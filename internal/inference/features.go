// Package inference provides feature engineering for TFT and RL models.
// Builds input tensors per phase.md lines 237-278.
package inference

import (
	"math"
	"time"
)

const (
	// TFTHistorySteps is the number of historical steps for TFT (12 = 2 hours @ 10min)
	// Matches Python max_encoder_length in vortex/brain/model.py
	TFTHistorySteps = 12

	// TFTFeatureCount is the number of features per timestep
	// TFT V2: 10 dimensions [spot, od, lag1, lag3, vol, hour, day, weekend, rel_time, enc_len]
	TFTFeatureCount = 10

	// RLFeatureCount is the number of state features for RL
	// REV 9: 13 dimensions including runtime risk score and fleet ratios.
	RLFeatureCount = 13

	// PriceVolatilityWindow is the rolling window size for volatility (steps).
	PriceVolatilityWindow = 12
)

// RingBuffer stores rolling price history.
type RingBuffer struct {
	data  []float64
	size  int
	pos   int
	count int
}

// NewRingBuffer creates a new ring buffer of the specified size.
func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		data: make([]float64, size),
		size: size,
	}
}

// Push adds a value to the buffer.
func (r *RingBuffer) Push(value float64) {
	r.data[r.pos] = value
	r.pos = (r.pos + 1) % r.size
	if r.count < r.size {
		r.count++
	}
}

// ToSlice returns all values in chronological order.
func (r *RingBuffer) ToSlice() []float64 {
	if r.count == 0 {
		return nil
	}

	result := make([]float64, r.count)
	if r.count < r.size {
		copy(result, r.data[:r.count])
	} else {
		// Ring is full, copy from pos to end then start to pos
		copy(result, r.data[r.pos:])
		copy(result[r.size-r.pos:], r.data[:r.pos])
	}
	return result
}

// Len returns the number of values in the buffer.
func (r *RingBuffer) Len() int {
	return r.count
}

// NodeState contains all state needed for feature engineering.
type NodeState struct {
	// Price data
	SpotPrice     float64
	OnDemandPrice float64
	PriceHistory  []float64 // Historical spot prices (TFTHistorySteps)

	// Node metrics
	CPUUsage    float64 // 0-1
	MemoryUsage float64 // 0-1

	// Workload info
	PodStartupTime     float64 // Max seconds for pods on node
	OutagePenaltyHours float64 // Max penalty hours from annotations
	MigrationCost      float64 // Estimated migration cost
	PriorityScore      float64 // Workload priority score (0-1)

	// Cluster state
	ClusterUtilization float64 // 0-1
	TimeSinceMigration int     // Steps since last migration
	RuntimeScore       float64 // 0-1, runtime interruption risk
	IsSpot             bool    // Current mode (The Missing Link)
	CurrentSpotRatio   float64 // V2: Fraction of node group on spot
	TargetSpotRatio    float64 // V2: Action-driven target

	// Timing
	Timestamp time.Time
}

// FeatureBuilder builds input tensors for TFT and RL models.
type FeatureBuilder struct {
	priceHistories map[string]*RingBuffer // key: node ID
}

// NewFeatureBuilder creates a new feature builder.
func NewFeatureBuilder() *FeatureBuilder {
	return &FeatureBuilder{
		priceHistories: make(map[string]*RingBuffer),
	}
}

// UpdatePriceHistory adds a new price point for a node.
func (f *FeatureBuilder) UpdatePriceHistory(nodeID string, price float64) {
	if _, ok := f.priceHistories[nodeID]; !ok {
		f.priceHistories[nodeID] = NewRingBuffer(TFTHistorySteps)
	}
	f.priceHistories[nodeID].Push(price)
}

// BuildTFTInput constructs the TFT input tensor.
// Per phase.md lines 237-260:
// Shape: [1, TFTHistorySteps, TFTFeatureCount] = [batch, history, features]
// Features: spot_price, on_demand_price, price_lag_1, price_lag_3, volatility, hour, day, weekend, rel_time, enc_len
func (f *FeatureBuilder) BuildTFTInput(nodeID string, state NodeState) []float32 {
	input := make([]float32, 1*TFTHistorySteps*TFTFeatureCount)

	// Get or create price history
	history := state.PriceHistory
	if len(history) < TFTHistorySteps {
		// Pad with current price if not enough history
		padded := make([]float64, TFTHistorySteps)
		offset := TFTHistorySteps - len(history)
		for i := 0; i < offset; i++ {
			padded[i] = state.SpotPrice
		}
		copy(padded[offset:], history)
		history = padded
	} else if len(history) > TFTHistorySteps {
		history = history[len(history)-TFTHistorySteps:]
	}

	// Calculate rolling volatility
	volatility := calculateRollingStd(history)

	// Time features
	hour := float64(state.Timestamp.Hour())
	dayOfWeek := float64(state.Timestamp.Weekday())
	isWeekend := 0.0
	if state.Timestamp.Weekday() == time.Saturday || state.Timestamp.Weekday() == time.Sunday {
		isWeekend = 1.0
	}
	// Build tensor for each timestep
	for t := 0; t < TFTHistorySteps; t++ {
		offset := t * TFTFeatureCount

		// Feature 0: spot_price
		input[offset+0] = float32(history[t])

		// Feature 1: ondemand_price
		input[offset+1] = float32(state.OnDemandPrice)

		// Feature 2: price_lag_1
		lag1 := history[t]
		if t > 0 {
			lag1 = history[t-1]
		}
		input[offset+2] = float32(lag1)

		// Feature 3: price_lag_3
		lag3 := history[t]
		if t >= 3 {
			lag3 = history[t-3]
		}
		input[offset+3] = float32(lag3)

		// Feature 4: Price volatility
		input[offset+4] = float32(volatility[t])

		// Feature 5: Hour of day (normalized 0-1)
		input[offset+5] = float32(hour / 24.0)

		// Feature 6: Day of week (normalized 0-1)
		input[offset+6] = float32(dayOfWeek / 7.0)

		// Feature 7: Is weekend
		input[offset+7] = float32(isWeekend)

		// Feature 8: relative_time_idx (current step relative to start of window)
		input[offset+8] = float32(t - (TFTHistorySteps - 1))

		// Feature 9: encoder_length (fixed at TFTHistorySteps)
		input[offset+9] = float32(TFTHistorySteps)
	}

	return input
}

// BuildRLInput constructs the RL state vector.
// Per phase.md and vortex/brain/rl/environment.py (V6 13-dim schema)
// Shape: [1, 13] = [batch, features]
// Normalization matches Python environment.py
func (f *FeatureBuilder) BuildRLInput(state NodeState, capacityScore float64) []float32 {
	input := make([]float32, RLFeatureCount)

	// Calculate current volatility (rolling window)
	var volatility float64
	if len(state.PriceHistory) > 1 {
		window := state.PriceHistory
		if len(window) > PriceVolatilityWindow {
			window = window[len(window)-PriceVolatilityWindow:]
		}
		volatility = calculateStdDev(window)
	}

	// Helper for safe normalization (0-1)
	norm := func(val float64, divisor float64) float32 {
		v := val / divisor
		if v < 0 {
			return 0
		}
		if v > 1 {
			return 1
		}
		return float32(v)
	}

	// Feature 0: spot_price (Norm / 100)
	input[0] = norm(state.SpotPrice, 100.0)

	// Feature 1: ondemand_price (Norm / 100)
	input[1] = norm(state.OnDemandPrice, 100.0)

	// Feature 2: price_volatility (Norm / 1.0 - Matches Python Clip)
	input[2] = norm(volatility, 1.0)

	// Feature 3: capacity_score (Raw 0-1 from TFT)
	input[3] = norm(capacityScore, 1.0)

	// Feature 4: runtime_score (Raw 0-1 runtime risk signal)
	input[4] = norm(state.RuntimeScore, 1.0)

	// Feature 5: pod_startup_time (Norm / 300s)
	input[5] = norm(state.PodStartupTime, 300.0)

	// Feature 6: migration_cost (Norm / 10.0)
	input[6] = norm(state.MigrationCost, 10.0)

	// Feature 7: cluster_utilization (Already 0-1)
	input[7] = norm(state.ClusterUtilization, 1.0)

	// Feature 8: time_since_migration (Steps, Norm / 100)
	tsm := float64(state.TimeSinceMigration)
	input[8] = norm(tsm, 100.0)

	// Feature 9: outage_penalty_hours (Norm / 10.0)
	input[9] = norm(state.OutagePenaltyHours, 10.0)

	// Feature 10: is_spot (0 or 1)
	if state.IsSpot {
		input[10] = 1.0
	} else {
		input[10] = 0.0
	}

	// Feature 11: current_spot_ratio (0-1)
	input[11] = norm(state.CurrentSpotRatio, 1.0)

	// Feature 12: target_spot_ratio (0-1)
	input[12] = norm(state.TargetSpotRatio, 1.0)

	return input
}

// calculateRollingStd computes rolling standard deviation for each timestep.
func calculateRollingStd(prices []float64) []float64 {
	result := make([]float64, len(prices))
	windowSize := PriceVolatilityWindow

	for i := range prices {
		start := i - windowSize + 1
		if start < 0 {
			start = 0
		}

		window := prices[start : i+1]
		result[i] = calculateStdDev(window)
	}

	return result
}

// calculateStdDev computes standard deviation of a slice.
func calculateStdDev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}

	// Mean
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))

	// Variance
	var variance float64
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(values) - 1)

	return math.Sqrt(variance)
}
