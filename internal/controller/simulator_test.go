package controller

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// InterruptionSimulator simulates AWS Spot interruption signals.
// Used for testing the agent's reaction time and success rate.
type InterruptionSimulator struct {
	mu     sync.Mutex
	logger *slog.Logger

	// Simulation results
	events      []InterruptionEvent
	totalEvents int
	successful  int
	failed      int
}

// InterruptionEvent represents a simulated interruption.
type InterruptionEvent struct {
	NodeID           string
	InterruptionTime time.Time
	DetectionTime    time.Time
	DrainStartTime   time.Time
	DrainEndTime     time.Time
	ReactionTimeMs   int64
	DrainTimeMs      int64
	Success          bool
	FailureReason    string
}

// NewInterruptionSimulator creates a new simulator.
func NewInterruptionSimulator(logger *slog.Logger) *InterruptionSimulator {
	if logger == nil {
		logger = slog.Default()
	}
	return &InterruptionSimulator{
		logger: logger,
		events: []InterruptionEvent{},
	}
}

// SimulateInterruption simulates a 120-second AWS interruption window.
// AWS provides a 2-minute (120s) warning before termination.
// The agent must detect and drain within this window.
func (s *InterruptionSimulator) SimulateInterruption(
	ctx context.Context,
	nodeID string,
	detectFn func(ctx context.Context, nodeID string) error,
	drainFn func(ctx context.Context, nodeID string) error,
) InterruptionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	event := InterruptionEvent{
		NodeID:           nodeID,
		InterruptionTime: time.Now(),
	}

	s.logger.Info("simulating AWS Spot interruption",
		"node_id", nodeID,
		"deadline_seconds", 120,
	)

	// Create deadline context (120 seconds per AWS spec)
	deadline := 120 * time.Second
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// Phase 1: Detection
	detectionStart := time.Now()
	if err := detectFn(ctx, nodeID); err != nil {
		event.FailureReason = "detection_failed: " + err.Error()
		event.Success = false
		s.failed++
		s.events = append(s.events, event)
		return event
	}
	event.DetectionTime = time.Now()
	event.ReactionTimeMs = time.Since(detectionStart).Milliseconds()

	s.logger.Info("interruption detected",
		"node_id", nodeID,
		"reaction_time_ms", event.ReactionTimeMs,
	)

	// Phase 2: Drain
	event.DrainStartTime = time.Now()
	if err := drainFn(ctx, nodeID); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			event.FailureReason = "drain_exceeded_120s_deadline"
		} else {
			event.FailureReason = "drain_failed: " + err.Error()
		}
		event.Success = false
		s.failed++
		s.events = append(s.events, event)
		return event
	}
	event.DrainEndTime = time.Now()
	event.DrainTimeMs = event.DrainEndTime.Sub(event.DrainStartTime).Milliseconds()

	// Verify within deadline
	totalTime := event.DrainEndTime.Sub(event.InterruptionTime)
	if totalTime > deadline {
		event.FailureReason = "exceeded_120s_deadline"
		event.Success = false
		s.failed++
	} else {
		event.Success = true
		s.successful++
	}

	s.totalEvents++
	s.events = append(s.events, event)

	s.logger.Info("interruption simulation complete",
		"node_id", nodeID,
		"success", event.Success,
		"total_time_ms", totalTime.Milliseconds(),
		"reaction_time_ms", event.ReactionTimeMs,
		"drain_time_ms", event.DrainTimeMs,
	)

	return event
}

// GetResults returns simulation statistics.
func (s *InterruptionSimulator) GetResults() SimulationResults {
	s.mu.Lock()
	defer s.mu.Unlock()

	var totalReactionTime, totalDrainTime int64
	for _, e := range s.events {
		totalReactionTime += e.ReactionTimeMs
		totalDrainTime += e.DrainTimeMs
	}

	count := len(s.events)
	if count == 0 {
		count = 1 // Avoid division by zero
	}

	return SimulationResults{
		TotalEvents:       s.totalEvents,
		Successful:        s.successful,
		Failed:            s.failed,
		SuccessRate:       float64(s.successful) / float64(max(s.totalEvents, 1)) * 100,
		AvgReactionTimeMs: totalReactionTime / int64(count),
		AvgDrainTimeMs:    totalDrainTime / int64(count),
		Events:            s.events,
	}
}

// SimulationResults contains aggregated simulation statistics.
type SimulationResults struct {
	TotalEvents       int
	Successful        int
	Failed            int
	SuccessRate       float64
	AvgReactionTimeMs int64
	AvgDrainTimeMs    int64
	Events            []InterruptionEvent
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- Unit Tests ---

// TestInterruptionSimulator_Success tests successful detection and drain.
func TestInterruptionSimulator_Success(t *testing.T) {
	sim := NewInterruptionSimulator(nil)

	// Mock detection and drain that complete quickly
	detectFn := func(ctx context.Context, nodeID string) error {
		time.Sleep(10 * time.Millisecond) // Simulate detection latency
		return nil
	}

	drainFn := func(ctx context.Context, nodeID string) error {
		time.Sleep(50 * time.Millisecond) // Simulate drain time
		return nil
	}

	event := sim.SimulateInterruption(
		context.Background(),
		"test-node-1",
		detectFn,
		drainFn,
	)

	if !event.Success {
		t.Errorf("expected success, got failure: %s", event.FailureReason)
	}

	results := sim.GetResults()
	if results.SuccessRate != 100.0 {
		t.Errorf("expected 100%% success rate, got %.2f%%", results.SuccessRate)
	}

	if results.AvgReactionTimeMs < 10 {
		t.Error("reaction time should be at least 10ms")
	}
}

// TestInterruptionSimulator_Deadline tests deadline exceeded scenario.
func TestInterruptionSimulator_Deadline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping deadline test in short mode")
	}

	sim := NewInterruptionSimulator(nil)

	// Use a very short timeout for testing
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	detectFn := func(ctx context.Context, nodeID string) error {
		return nil
	}

	drainFn := func(ctx context.Context, nodeID string) error {
		// Simulate slow drain that exceeds deadline
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return nil
		}
	}

	event := sim.SimulateInterruption(
		ctx,
		"slow-node",
		detectFn,
		drainFn,
	)

	if event.Success {
		t.Error("expected failure due to deadline exceeded")
	}
}

// TestInterruptionSimulator_MultipleNodes tests batch interruption.
func TestInterruptionSimulator_MultipleNodes(t *testing.T) {
	sim := NewInterruptionSimulator(nil)

	detectFn := func(ctx context.Context, nodeID string) error {
		return nil
	}

	drainFn := func(ctx context.Context, nodeID string) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	}

	// Simulate 5 nodes being interrupted
	for i := 0; i < 5; i++ {
		sim.SimulateInterruption(
			context.Background(),
			"node-"+string(rune('a'+i)),
			detectFn,
			drainFn,
		)
	}

	results := sim.GetResults()
	if results.TotalEvents != 5 {
		t.Errorf("expected 5 events, got %d", results.TotalEvents)
	}

	if results.Successful != 5 {
		t.Errorf("expected 5 successful, got %d", results.Successful)
	}

	t.Logf("Simulation Results: %d events, %.2f%% success rate, avg reaction: %dms, avg drain: %dms",
		results.TotalEvents,
		results.SuccessRate,
		results.AvgReactionTimeMs,
		results.AvgDrainTimeMs,
	)
}
