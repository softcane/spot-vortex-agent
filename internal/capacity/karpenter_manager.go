package capacity

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/karpenter"
)

// KarpenterManager implements CapacityManager for Karpenter-provisioned nodes.
//
// Swap strategy:
//   - PrepareSwap: steers NodePool weights (fast API patch, non-blocking).
//   - PostDrainCleanup: no-op (Karpenter manages node lifecycle after drain).
//
// Per integration_strategy.md Section 3: Karpenter is the "happy path" -
// most aligned with SpotVortex's node-first design.
type KarpenterManager struct {
	nodePoolMgr *karpenter.NodePoolManager
	logger      *slog.Logger

	// Config
	spotSuffix string
	odSuffix   string
	spotWeight int32
	odWeight   int32

	// Cooldown state
	mu               sync.Mutex
	lastWeightChange map[string]time.Time
	cooldown         time.Duration
}

// KarpenterManagerConfig configures the Karpenter capacity manager.
type KarpenterManagerConfig struct {
	NodePoolManager        *karpenter.NodePoolManager
	Logger                 *slog.Logger
	SpotNodePoolSuffix     string
	OnDemandNodePoolSuffix string
	SpotWeight             int32
	OnDemandWeight         int32
	CooldownSeconds        int
}

// NewKarpenterManager creates a new Karpenter capacity manager.
func NewKarpenterManager(cfg KarpenterManagerConfig) *KarpenterManager {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SpotNodePoolSuffix == "" {
		cfg.SpotNodePoolSuffix = "-spot"
	}
	if cfg.OnDemandNodePoolSuffix == "" {
		cfg.OnDemandNodePoolSuffix = "-od"
	}
	if cfg.SpotWeight == 0 {
		cfg.SpotWeight = 100
	}
	if cfg.OnDemandWeight == 0 {
		cfg.OnDemandWeight = 10
	}
	cooldown := time.Duration(cfg.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}

	return &KarpenterManager{
		nodePoolMgr:      cfg.NodePoolManager,
		logger:           cfg.Logger,
		spotSuffix:       cfg.SpotNodePoolSuffix,
		odSuffix:         cfg.OnDemandNodePoolSuffix,
		spotWeight:       cfg.SpotWeight,
		odWeight:         cfg.OnDemandWeight,
		lastWeightChange: make(map[string]time.Time),
		cooldown:         cooldown,
	}
}

func (m *KarpenterManager) Type() ManagerType {
	return ManagerKarpenter
}

func (m *KarpenterManager) PrepareSwap(ctx context.Context, pool PoolInfo, direction SwapDirection) (*SwapResult, error) {
	start := time.Now()

	if m.nodePoolMgr == nil {
		return nil, fmt.Errorf("karpenter NodePoolManager not configured")
	}

	// Check cooldown
	m.mu.Lock()
	lastChange, hasLast := m.lastWeightChange[pool.Name]
	m.mu.Unlock()

	if hasLast && time.Since(lastChange) < m.cooldown {
		m.logger.Info("skipping weight steering due to cooldown",
			"pool", pool.Name,
			"remaining", m.cooldown-time.Since(lastChange),
		)
		return &SwapResult{Ready: true, Duration: time.Since(start)}, nil
	}

	// Determine weights based on direction
	spotPoolName := pool.Name + m.spotSuffix
	odPoolName := pool.Name + m.odSuffix

	var spotWeight, odWeight int32
	switch direction {
	case SwapToOnDemand:
		// Favor OD: high weight on OD, low on spot
		spotWeight = m.odWeight
		odWeight = m.spotWeight
	case SwapToSpot:
		// Favor spot: high weight on spot, low on OD
		spotWeight = m.spotWeight
		odWeight = m.odWeight
	}

	m.logger.Info("steering Karpenter weights",
		"pool", pool.Name,
		"direction", direction.String(),
		"spot_pool", spotPoolName,
		"od_pool", odPoolName,
		"spot_weight", spotWeight,
		"od_weight", odWeight,
	)

	// Patch both NodePool weights
	spotErr := m.nodePoolMgr.SetWeight(ctx, spotPoolName, spotWeight)
	odErr := m.nodePoolMgr.SetWeight(ctx, odPoolName, odWeight)

	if spotErr != nil {
		m.logger.Warn("failed to set spot NodePool weight", "pool", spotPoolName, "error", spotErr)
	}
	if odErr != nil {
		m.logger.Warn("failed to set OD NodePool weight", "pool", odPoolName, "error", odErr)
	}

	// Record weight change time if at least one succeeded
	if spotErr == nil || odErr == nil {
		m.mu.Lock()
		m.lastWeightChange[pool.Name] = time.Now()
		m.mu.Unlock()
	}

	return &SwapResult{
		Ready:    spotErr == nil || odErr == nil,
		Duration: time.Since(start),
	}, nil
}

func (m *KarpenterManager) PostDrainCleanup(ctx context.Context, nodeName string, pool PoolInfo) error {
	// Karpenter manages node lifecycle after drain - no cleanup needed.
	m.logger.Debug("karpenter post-drain cleanup (no-op)", "node", nodeName, "pool", pool.Name)
	return nil
}

func (m *KarpenterManager) IsAvailable(ctx context.Context) bool {
	if m.nodePoolMgr == nil {
		return false
	}
	return m.nodePoolMgr.IsKarpenterAvailable(ctx)
}

// Compile-time interface check.
var _ CapacityManager = (*KarpenterManager)(nil)
