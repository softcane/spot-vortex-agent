// Package billing provides savings metering for SpotVortex.
//
// Tracks node uptime on Spot instances and reports savings to the billing API.
// Commission: 10% of (OnDemand - Spot) * Uptime
//
// Architecture: architecture.md (Stripe Metering API)
// Best Practices: best_practices.md (Metered Accuracy verified every 24h)
package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// SavingsEvent represents a metered savings event.
type SavingsEvent struct {
	NodeID        string    `json:"node_id"`
	InstanceType  string    `json:"instance_type"`
	Region        string    `json:"region"`
	Zone          string    `json:"zone"`
	SpotPrice     float64   `json:"spot_price"`
	OnDemandPrice float64   `json:"ondemand_price"`
	UptimeMinutes int       `json:"uptime_minutes"`
	Savings       float64   `json:"savings"` // (OnDemand - Spot) * Uptime / 60
	Timestamp     time.Time `json:"timestamp"`
}

// Meter tracks and reports savings to the billing API.
type Meter struct {
	endpoint string
	enabled  bool
	dryRun   bool
	logger   *slog.Logger

	// Track active nodes
	mu          sync.Mutex
	activeNodes map[string]nodeTracker

	// HTTP client with timeout
	client *http.Client
}

type nodeTracker struct {
	StartTime     time.Time
	InstanceType  string
	Region        string
	Zone          string
	SpotPrice     float64
	OnDemandPrice float64
}

// MeterConfig holds configuration for the billing meter.
type MeterConfig struct {
	Endpoint string
	Enabled  bool
	DryRun   bool
	Logger   *slog.Logger
}

// NewMeter creates a new billing meter.
func NewMeter(cfg MeterConfig) *Meter {
	return &Meter{
		endpoint:    cfg.Endpoint,
		enabled:     cfg.Enabled,
		dryRun:      cfg.DryRun,
		logger:      cfg.Logger,
		activeNodes: make(map[string]nodeTracker),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// TrackNodeStart records when a Spot node becomes active.
func (m *Meter) TrackNodeStart(nodeID, instanceType, region, zone string, spotPrice, onDemandPrice float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.activeNodes[nodeID] = nodeTracker{
		StartTime:     time.Now(),
		InstanceType:  instanceType,
		Region:        region,
		Zone:          zone,
		SpotPrice:     spotPrice,
		OnDemandPrice: onDemandPrice,
	}

	m.logger.Info("tracking node for billing",
		"node_id", nodeID,
		"instance_type", instanceType,
		"zone", zone,
		"spot_price", spotPrice,
		"ondemand_price", onDemandPrice,
	)
}

// TrackNodeEnd records when a Spot node is drained and reports savings.
func (m *Meter) TrackNodeEnd(ctx context.Context, nodeID string) error {
	m.mu.Lock()
	tracker, exists := m.activeNodes[nodeID]
	if exists {
		delete(m.activeNodes, nodeID)
	}
	m.mu.Unlock()

	if !exists {
		m.logger.Warn("node not tracked for billing", "node_id", nodeID)
		return nil
	}

	// Calculate savings
	uptime := time.Since(tracker.StartTime)
	uptimeMinutes := int(uptime.Minutes())
	hourlyRate := tracker.OnDemandPrice - tracker.SpotPrice
	savings := hourlyRate * (float64(uptimeMinutes) / 60.0)

	event := SavingsEvent{
		NodeID:        nodeID,
		InstanceType:  tracker.InstanceType,
		Region:        tracker.Region,
		Zone:          tracker.Zone,
		SpotPrice:     tracker.SpotPrice,
		OnDemandPrice: tracker.OnDemandPrice,
		UptimeMinutes: uptimeMinutes,
		Savings:       savings,
		Timestamp:     time.Now(),
	}

	m.logger.Info("savings event generated",
		"node_id", nodeID,
		"uptime_minutes", uptimeMinutes,
		"savings", savings,
		"zone", tracker.Zone,
	)

	return m.ReportSavings(ctx, event)
}

// ReportSavings sends a savings event to the billing API.
func (m *Meter) ReportSavings(ctx context.Context, event SavingsEvent) error {
	if !m.enabled {
		m.logger.Debug("billing disabled, skipping report")
		return nil
	}

	if m.dryRun {
		m.logger.Info("DRY-RUN: would report savings",
			"node_id", event.NodeID,
			"savings", event.Savings,
			"endpoint", m.endpoint,
		)
		return nil
	}

	// Marshal event
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal savings event: %w", err)
	}

	// Send to billing API
	req, err := http.NewRequestWithContext(ctx, "POST", m.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	// Auth removed for free version
	req.Header.Set("X-SpotVortex-Version", "1.1.0")

	resp, err := m.client.Do(req)
	if err != nil {
		m.logger.Error("failed to report savings", "error", err)
		return fmt.Errorf("failed to send savings event: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		m.logger.Error("billing API error",
			"status", resp.StatusCode,
			"node_id", event.NodeID,
		)
		return fmt.Errorf("billing API returned status %d", resp.StatusCode)
	}

	m.logger.Info("savings reported successfully",
		"node_id", event.NodeID,
		"savings", event.Savings,
	)

	return nil
}

// GetActiveSavings returns estimated savings for all currently tracked nodes.
func (m *Meter) GetActiveSavings() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	var total float64
	now := time.Now()

	for _, tracker := range m.activeNodes {
		uptime := now.Sub(tracker.StartTime)
		hourlyRate := tracker.OnDemandPrice - tracker.SpotPrice
		savings := hourlyRate * (uptime.Hours())
		total += savings
	}

	return total
}
