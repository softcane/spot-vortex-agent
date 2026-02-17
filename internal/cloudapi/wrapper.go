package cloudapi

import (
	"context"
	"log/slog"
	"time"
)

// SpotWrapper wraps a real cloud provider with safety controls.
// It enforces dry-run mode and provides structured logging for all operations.
type SpotWrapper struct {
	dryRun   bool
	provider CloudProvider // The underlying real provider (nil in dry-run only mode)
	logger   *slog.Logger
}

// SpotWrapperConfig configures the SpotWrapper.
type SpotWrapperConfig struct {
	DryRun   bool
	Provider CloudProvider
	Logger   *slog.Logger
}

// NewSpotWrapper creates a new safety wrapper for cloud operations.
func NewSpotWrapper(cfg SpotWrapperConfig) *SpotWrapper {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &SpotWrapper{
		dryRun:   cfg.DryRun,
		provider: cfg.Provider,
		logger:   logger,
	}
}

// Drain implements CloudProvider.Drain with safety wrapper.
func (w *SpotWrapper) Drain(ctx context.Context, req DrainRequest) (*DrainResult, error) {
	start := time.Now()

	w.logger.Info("drain requested",
		"node_id", req.NodeID,
		"zone", req.Zone,
		"grace_period", req.GracePeriod,
		"force", req.Force,
		"dry_run", w.dryRun,
	)

	// Dry-run mode: log and return simulated success
	if w.dryRun {
		w.logger.Info("dry-run: simulating drain",
			"node_id", req.NodeID,
			"zone", req.Zone,
			"risk_score", 0.0, // Would be populated by controller
			"action", "would_drain_node",
		)

		return &DrainResult{
			NodeID:      req.NodeID,
			Success:     true,
			DryRun:      true,
			Duration:    time.Since(start),
			PodsEvicted: 0,
		}, nil
	}

	// Real execution path
	if w.provider == nil {
		w.logger.Error("no cloud provider configured for live mode")
		return nil, ErrNoProvider
	}

	return w.provider.Drain(ctx, req)
}

// Provision implements CloudProvider.Provision with safety wrapper.
func (w *SpotWrapper) Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error) {
	w.logger.Info("provision requested",
		"instance_type", req.InstanceType,
		"zone", req.Zone,
		"spot_price", req.SpotPrice,
		"fallback_on_demand", req.FallbackToOnDemand,
		"dry_run", w.dryRun,
	)

	// Dry-run mode: log and return simulated success
	if w.dryRun {
		w.logger.Info("dry-run: simulating provision",
			"zone", req.Zone,
			"instance_type", req.InstanceType,
			"risk_score", 0.0,
			"action", "would_provision_instance",
		)

		return &ProvisionResult{
			InstanceID:   "dry-run-instance-id",
			InstanceType: req.InstanceType,
			Zone:         req.Zone,
			IsSpot:       true,
			DryRun:       true,
		}, nil
	}

	// Real execution path
	if w.provider == nil {
		w.logger.Error("no cloud provider configured for live mode")
		return nil, ErrNoProvider
	}

	return w.provider.Provision(ctx, req)
}

// IsDryRun returns whether the wrapper is in dry-run mode.
func (w *SpotWrapper) IsDryRun() bool {
	return w.dryRun
}

// Compile-time interface check
var _ CloudProvider = (*SpotWrapper)(nil)
