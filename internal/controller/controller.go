// Package controller implements the SpotVortex reconciliation loop.
// It monitors predictions from the Vortex-Brain and takes action on at-risk nodes.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pradeepsingh/spot-vortex-agent/internal/cloudapi"
	"github.com/pradeepsingh/spot-vortex-agent/internal/collector"
	"github.com/pradeepsingh/spot-vortex-agent/internal/config"
	"github.com/pradeepsingh/spot-vortex-agent/internal/inference"
	"github.com/pradeepsingh/spot-vortex-agent/internal/karpenter"
	"github.com/pradeepsingh/spot-vortex-agent/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Controller manages the spot instance lifecycle.
type Controller struct {
	mu     sync.RWMutex
	cloud  cloudapi.CloudProvider
	priceP cloudapi.PriceProvider
	k8s    kubernetes.Interface
	inf    *inference.InferenceEngine
	prom   *metrics.Client
	drain  *Drainer
	coll   *collector.Collector
	logger *slog.Logger
	price  *priceSynth
	metric *metricSynth

	// Karpenter integration (per PRODUCTION_FLOW_EKS_KARPENTER.md)
	nodePoolMgr   *karpenter.NodePoolManager
	karpenterCfg  config.KarpenterConfig
	dynamicClient dynamic.Interface

	// Configuration - ALL values from config file, NO defaults
	riskThreshold       float64
	maxDrainRatio       float64
	reconcileInterval   time.Duration
	confidenceThreshold float64
	useSyntheticMetrics bool
	useSyntheticPrices  bool

	// State
	running         bool
	stopCh          chan struct{}
	historyLock     sync.Mutex
	priceHistory    map[string][]float64
	lastMigration   map[string]time.Time
	targetSpotRatio map[string]float64
	// currentSpotRatio tracks current spot ratio per pool (computed from actual nodes)
	currentSpotRatio map[string]float64
	// poolNodeCounts tracks node counts per pool for drain calculation
	poolNodeCounts map[string]*poolCount
	// lastWeightChange tracks when weights were last changed per workload pool (for cooldown)
	lastWeightChange map[string]time.Time
}

// poolCount tracks node counts per pool for drain calculation.
type poolCount struct {
	total int
	spot  int
}

// Config holds controller configuration.
type Config struct {
	Cloud                   cloudapi.CloudProvider
	PriceProvider           cloudapi.PriceProvider
	K8sClient               kubernetes.Interface
	DynamicClient           dynamic.Interface
	Inference               *inference.InferenceEngine
	PrometheusClient        *metrics.Client
	Logger                  *slog.Logger
	RiskThreshold           float64
	MaxDrainRatio           float64
	ReconcileInterval       time.Duration
	ConfidenceThreshold     float64
	DrainGracePeriodSeconds int64
	// Karpenter configuration (per PRODUCTION_FLOW_EKS_KARPENTER.md)
	Karpenter config.KarpenterConfig
}

// New creates a new Controller instance.
// All configuration values are REQUIRED - no defaults.
func New(cfg Config) (*Controller, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Validate required components - NO mock fallbacks
	if cfg.PrometheusClient == nil {
		return nil, fmt.Errorf("prometheus client is required")
	}
	if cfg.Inference == nil {
		return nil, fmt.Errorf("inference engine is required")
	}
	if cfg.Cloud == nil {
		return nil, fmt.Errorf("cloud provider is required")
	}

	// Validate config values - NO hardcoded defaults
	if cfg.RiskThreshold <= 0 || cfg.RiskThreshold > 1 {
		return nil, fmt.Errorf("riskThreshold must be between 0 and 1")
	}
	if cfg.MaxDrainRatio <= 0 || cfg.MaxDrainRatio > 1 {
		return nil, fmt.Errorf("maxDrainRatio must be between 0 and 1")
	}
	if cfg.ReconcileInterval < 10*time.Second {
		return nil, fmt.Errorf("reconcileInterval must be >= 10s")
	}
	if cfg.ConfidenceThreshold <= 0 || cfg.ConfidenceThreshold > 1 {
		return nil, fmt.Errorf("confidenceThreshold must be between 0 and 1")
	}

	useSyntheticMetrics := strings.EqualFold(os.Getenv("SPOTVORTEX_METRICS_MODE"), "synthetic")
	useSyntheticPrices := strings.EqualFold(os.Getenv("SPOTVORTEX_PRICE_MODE"), "synthetic")
	if (useSyntheticMetrics || useSyntheticPrices) && !cfg.Cloud.IsDryRun() {
		return nil, fmt.Errorf("synthetic telemetry modes are blocked when dry-run is disabled: SPOTVORTEX_METRICS_MODE=%q SPOTVORTEX_PRICE_MODE=%q", os.Getenv("SPOTVORTEX_METRICS_MODE"), os.Getenv("SPOTVORTEX_PRICE_MODE"))
	}

	var priceSynth *priceSynth
	if useSyntheticPrices {
		priceSynth = newPriceSynth(time.Now().UnixNano())
	}

	var metricSynth *metricSynth
	if useSyntheticMetrics {
		metricSynth = newMetricSynth(time.Now().UnixNano() + 1)
	}

	var drainer *Drainer
	if cfg.K8sClient != nil {
		drainer = NewDrainer(cfg.K8sClient, logger, DrainConfig{
			GracePeriodSeconds: cfg.DrainGracePeriodSeconds,
			Timeout:            5 * time.Minute,
			IgnoreDaemonSets:   true,
			DeleteEmptyDirData: true,
		})
	}

	// Initialize Karpenter NodePoolManager if enabled and dynamic client provided
	var nodePoolMgr *karpenter.NodePoolManager
	if cfg.Karpenter.Enabled && cfg.DynamicClient != nil {
		nodePoolMgr = karpenter.NewNodePoolManager(cfg.DynamicClient, logger)
		logger.Info("Karpenter integration enabled",
			"spot_suffix", cfg.Karpenter.SpotNodePoolSuffix,
			"od_suffix", cfg.Karpenter.OnDemandNodePoolSuffix,
			"spot_weight", cfg.Karpenter.SpotWeight,
			"od_weight", cfg.Karpenter.OnDemandWeight,
		)
	}

	return &Controller{
		cloud:               cfg.Cloud,
		priceP:              cfg.PriceProvider,
		k8s:                 cfg.K8sClient,
		dynamicClient:       cfg.DynamicClient,
		inf:                 cfg.Inference,
		prom:                cfg.PrometheusClient,
		drain:               drainer,
		coll:                collector.NewCollector(cfg.K8sClient, logger), // Wiring Collector
		logger:              logger,
		price:               priceSynth,
		metric:              metricSynth,
		nodePoolMgr:         nodePoolMgr,
		karpenterCfg:        cfg.Karpenter,
		riskThreshold:       cfg.RiskThreshold,
		maxDrainRatio:       cfg.MaxDrainRatio,
		reconcileInterval:   cfg.ReconcileInterval,
		confidenceThreshold: cfg.ConfidenceThreshold,
		useSyntheticMetrics: useSyntheticMetrics,
		useSyntheticPrices:  useSyntheticPrices,
		stopCh:              make(chan struct{}),
		priceHistory:        make(map[string][]float64),
		lastMigration:       make(map[string]time.Time),
		targetSpotRatio:     make(map[string]float64),
		currentSpotRatio:    make(map[string]float64),
		poolNodeCounts:      make(map[string]*poolCount),
		lastWeightChange:    make(map[string]time.Time),
	}, nil
}

// Start begins the controller's main loop.
// Runs reconciliation every 30 seconds (configurable).
func (c *Controller) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.running = true
	c.mu.Unlock()

	c.logger.Info("controller starting",
		"risk_threshold", c.riskThreshold,
		"max_drain_ratio", c.maxDrainRatio,
		"reconcile_interval", c.reconcileInterval,
		"dry_run", c.cloud.IsDryRun(),
		"synthetic_metrics", c.useSyntheticMetrics,
		"synthetic_prices", c.useSyntheticPrices,
	)

	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()

	// Run initial reconciliation
	if err := c.Reconcile(ctx); err != nil {
		c.logger.Error("initial reconciliation failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("controller stopped by context")
			return ctx.Err()
		case <-c.stopCh:
			c.logger.Info("controller stopped")
			return nil
		case <-ticker.C:
			if err := c.Reconcile(ctx); err != nil {
				c.logger.Error("reconciliation failed", "error", err)
			}
		}
	}
}

// Stop stops the controller.
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		close(c.stopCh)
		c.running = false
	}
}

// Reconcile represents a single reconciliation cycle.
// It checks predictions and takes action on at-risk nodes.
// In dry-run mode, it logs potential savings without executing drains.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	isDryRun := c.cloud != nil && c.cloud.IsDryRun()
	c.logger.Debug("starting reconciliation cycle", "dry_run", isDryRun)

	// Step 1: Get current node metrics from Prometheus
	nodeMetrics, err := c.fetchNodeMetrics(ctx)
	if err != nil {
		c.logger.Warn("failed to fetch node metrics, skipping cycle", "error", err)
		return nil // Don't fail the whole cycle
	}

	if len(nodeMetrics) == 0 {
		c.logger.Debug("no nodes to assess")
		return nil
	}

	// Step 1.5: Refresh local workload metrics (Pod latency, PDBs, etc.)
	if _, err := c.coll.Collect(ctx); err != nil {
		c.logger.Warn("failed to collect workload metrics", "error", err)
		return nil // Skip cycle instead of inferring with stale/default workload features.
	}

	// Step 2: Run inference on each node
	// PRODUCTION MODE: Inference failure is fatal for that node - skip
	assessments, err := c.runInference(ctx, nodeMetrics)
	if err != nil {
		return fmt.Errorf("inference failure: %w", err)
	}

	// Step 2.5: In dry-run mode, generate and log savings report
	// This shows customers the potential value before enabling active management
	if isDryRun {
		c.generateAndLogSavingsReport(ctx, nodeMetrics, assessments)
	}

	// Step 3: Identify actionable nodes
	actionableNodes := c.filterActionableNodes(assessments)
	actionableNodes = c.filterExecutableNodes(ctx, actionableNodes)
	c.logger.Info("risk assessment complete",
		"total_nodes", len(assessments),
		"actionable_nodes", len(actionableNodes),
		"dry_run", isDryRun,
	)

	if len(actionableNodes) == 0 {
		return nil
	}

	// Step 4: Apply thundering herd protection (respects Karpenter disruption budgets)
	nodesToDrain := c.applyDrainLimit(ctx, actionableNodes, len(nodeMetrics))

	// Step 5: Batch steer Karpenter weights BEFORE draining
	// Per PRODUCTION_FLOW_EKS_KARPENTER.md: weights must be updated before drains
	// so that Karpenter provisions the correct capacity type for pending pods.
	// In dry-run mode, this logs but doesn't actually patch
	c.batchSteerKarpenterWeights(ctx, nodesToDrain)

	// Step 6: Execute actions (drain nodes)
	// In dry-run mode, drainer logs but doesn't actually evict pods
	for _, node := range nodesToDrain {
		if err := c.executeAction(ctx, node); err != nil {
			c.logger.Error("failed to execute action",
				"node_id", node.NodeID,
				"error", err,
			)
		}
	}

	c.logger.Debug("reconciliation cycle complete", "dry_run", isDryRun)
	return nil
}

// fetchNodeMetrics gets current metrics from Prometheus.
// REQUIRED: Prometheus client must be configured.
func (c *Controller) fetchNodeMetrics(ctx context.Context) ([]metrics.NodeMetrics, error) {
	if c.useSyntheticMetrics {
		return c.syntheticNodeMetrics(ctx)
	}
	// NO mock fallback - Prometheus is required
	return c.prom.GetNodeMetrics(ctx)
}

// NodeAssessment contains the inference result and recommended action for a node.
type NodeAssessment struct {
	NodeID        string
	Action        inference.Action
	CapacityScore float32
	RuntimeScore  float32
	Confidence    float32
}

func (c *Controller) unsupportedFamilyAssessment(nodeID, instanceType, reason string) NodeAssessment {
	family := inference.InstanceFamilyLabel(instanceType)
	c.logger.Warn(
		"forcing on-demand: instance family is not supported by current model at the moment",
		"node_id", nodeID,
		"instance_type", instanceType,
		"family", family,
		"reason", reason,
	)
	metrics.UnsupportedInstanceFamily.WithLabelValues(family).Inc()
	metrics.DecisionSource.WithLabelValues(
		"unsupported_family",
		inference.ActionToString(inference.ActionEmergencyExit),
	).Inc()

	return NodeAssessment{
		NodeID:        nodeID,
		Action:        inference.ActionEmergencyExit,
		CapacityScore: 1.0,
		RuntimeScore:  1.0,
		Confidence:    1.0,
	}
}

func (c *Controller) loadRuntimeConfig() *config.RuntimeConfig {
	runtimeCfg, err := config.LoadRuntimeConfig("config/runtime.json")
	if err != nil {
		return config.DefaultRuntimeConfig()
	}
	return runtimeCfg
}

func stepsSinceMigration(last time.Time, stepMinutes int) int {
	if stepMinutes <= 0 {
		stepMinutes = 10
	}
	steps := int(time.Since(last).Minutes() / float64(stepMinutes))
	if steps < 0 {
		return 0
	}
	return steps
}

// runInference runs the coordinated TFT/RL models on node metrics.
// If UsePoolLevelInference is enabled, delegates to runPoolLevelInference.
func (c *Controller) runInference(ctx context.Context, nodeMetrics []metrics.NodeMetrics) ([]NodeAssessment, error) {
	// Per PRODUCTION_FLOW_EKS_KARPENTER.md Section 6 Option 2:
	// Pool-level inference aggregation provides "one coherent action per pool per tick"
	if c.karpenterCfg.UsePoolLevelInference {
		return c.runPoolLevelInference(ctx, nodeMetrics)
	}

	runtimeCfg := c.loadRuntimeConfig()
	riskMult := runtimeCfg.RiskMultiplier
	stepMinutes := runtimeCfg.StepMinutes
	useDeterministic := runtimeCfg.UseDeterministicPolicy()

	assessments := make([]NodeAssessment, 0, len(nodeMetrics))

	// Get cluster-wide stats for feature building
	clusterUtil := c.calculateClusterUtilization(nodeMetrics)
	nodeInfo, err := c.nodeInfoMap(ctx)
	if err != nil {
		c.logger.Warn("failed to load node labels", "error", err)
	}

	// Track workload pool per node for extended pool ID support
	nodeWorkloadPool := make(map[string]string)

	poolCounts := make(map[string]*poolCount)
	resolved := make([]metrics.NodeMetrics, 0, len(nodeMetrics))
	for _, raw := range nodeMetrics {
		m := raw
		m.NodeID = strings.TrimSpace(m.NodeID)
		info, ok := nodeInfo[m.NodeID]
		if !ok {
			host := strings.Split(m.NodeID, ":")[0]
			info, ok = nodeInfo[host]
		}
		if ok {
			m.NodeID = info.name
			if m.Zone == "" {
				m.Zone = info.zone
			}
			if m.InstanceType == "" {
				m.InstanceType = info.instanceType
			}
			m.IsSpot = info.isSpot
			// Store workload pool for later use
			nodeWorkloadPool[m.NodeID] = info.workloadPool
		}

		// Identify Node Pool - use extended format if configured
		poolID := c.getPoolIDWithExtendedFormat(m.InstanceType, m.Zone, nodeWorkloadPool[m.NodeID])
		if m.InstanceType == "" || m.Zone == "" {
			poolID = "unknown:unknown"
		}
		counts := poolCounts[poolID]
		if counts == nil {
			counts = &poolCount{}
			poolCounts[poolID] = counts
		}
		counts.total++
		if m.IsSpot {
			counts.spot++
		}
		resolved = append(resolved, m)
	}

	currentSpotRatio := make(map[string]float64, len(poolCounts))
	for poolID, counts := range poolCounts {
		if counts.total > 0 {
			currentSpotRatio[poolID] = float64(counts.spot) / float64(counts.total)
		}
	}

	targetSpotRatio := make(map[string]float64, len(poolCounts))
	c.historyLock.Lock()
	// Update controller state with current pool counts and ratios
	c.poolNodeCounts = poolCounts
	c.currentSpotRatio = currentSpotRatio
	for poolID, ratio := range currentSpotRatio {
		if _, ok := c.targetSpotRatio[poolID]; !ok {
			c.targetSpotRatio[poolID] = ratio
		}
	}
	for poolID, ratio := range c.targetSpotRatio {
		targetSpotRatio[poolID] = ratio
	}
	c.historyLock.Unlock()

	priceCache := make(map[string]cloudapi.SpotPriceData, len(poolCounts))

	for _, m := range resolved {
		// Identify Node Pool - use extended format if configured
		poolID := c.getPoolIDWithExtendedFormat(m.InstanceType, m.Zone, nodeWorkloadPool[m.NodeID])
		if m.InstanceType == "" || m.Zone == "" {
			poolID = "unknown:unknown"
		}

		if supported, reason := c.inf.SupportsInstanceType(m.InstanceType); !supported {
			assessments = append(assessments, c.unsupportedFamilyAssessment(m.NodeID, m.InstanceType, reason))
			continue
		}

		var priceHistory []float64
		if c.price != nil {
			spotPrice, ondemandPrice, history := c.price.Next(m.NodeID, m.IsSpot)
			if m.SpotPrice <= 0 {
				m.SpotPrice = spotPrice
			}
			if m.OnDemandPrice <= 0 {
				m.OnDemandPrice = ondemandPrice
			}
			priceHistory = history
		} else if c.priceP != nil && m.InstanceType != "" && m.Zone != "" {
			if cached, ok := priceCache[poolID]; ok {
				if m.SpotPrice <= 0 {
					m.SpotPrice = cached.CurrentPrice
				}
				if m.OnDemandPrice <= 0 {
					m.OnDemandPrice = cached.OnDemandPrice
				}
				priceHistory = append([]float64(nil), cached.PriceHistory...)
			} else {
				data, err := c.priceP.GetSpotPrice(ctx, m.InstanceType, m.Zone)
				if err != nil {
					c.logger.Warn("failed to fetch spot price data",
						"pool", poolID,
						"error", err,
					)
				} else {
					priceCache[poolID] = data
					if m.SpotPrice <= 0 {
						m.SpotPrice = data.CurrentPrice
					}
					if m.OnDemandPrice <= 0 {
						m.OnDemandPrice = data.OnDemandPrice
					}
					priceHistory = append([]float64(nil), data.PriceHistory...)
				}
			}
		}

		if len(priceHistory) == 0 {
			// Real Mode: Maintain rolling history buffer
			c.historyLock.Lock()
			hist := c.priceHistory[poolID]
			// Only append if we have a valid price
			if m.SpotPrice > 0 {
				hist = append(hist, m.SpotPrice)
				if len(hist) > inference.TFTHistorySteps*2 {
					hist = hist[len(hist)-inference.TFTHistorySteps*2:]
				}
				c.priceHistory[poolID] = hist
			}
			// Copy for thread safety during inference
			priceHistory = make([]float64, len(hist))
			copy(priceHistory, hist)
			c.historyLock.Unlock()
		}

		priceHistory = normalizePriceHistory(priceHistory, m.SpotPrice)
		if m.SpotPrice <= 0 || m.OnDemandPrice <= 0 || len(priceHistory) == 0 {
			c.logger.Warn("skipping inference due to missing market telemetry",
				"node", m.NodeID,
				"instance_type", m.InstanceType,
				"zone", m.Zone,
				"spot_price", m.SpotPrice,
				"ondemand_price", m.OnDemandPrice,
				"history_points", len(priceHistory),
			)
			continue
		}

		poolFeats := c.coll.GetPoolFeatures(poolID)

		// Calculate Migration Cost (USD)
		// (startup_minutes / 60) * (savings_hourly) * utilization * multiplier (2.0)
		startupMins := poolFeats.PodStartupTime / 60.0
		hourlyDelta := maxFloat(0.0, m.OnDemandPrice-m.SpotPrice)
		migCost := (startupMins / 60.0) * hourlyDelta * clusterUtil * 2.0 // Multiplier=2.0

		// Calculate TimeSinceMigration (Steps)
		// Default to 100 steps (saturated normalized value) if unknown
		tsm := 100
		// Use historyLock for thread-safe access to migration state

		c.historyLock.Lock()
		if last, ok := c.lastMigration[poolID]; ok {
			tsm = stepsSinceMigration(last, stepMinutes)
		}
		c.historyLock.Unlock()

		currentRatio := currentSpotRatio[poolID]
		targetRatio := targetSpotRatio[poolID]

		// Map metrics.NodeMetrics to inference.NodeState
		state := inference.NodeState{
			SpotPrice:          m.SpotPrice,
			OnDemandPrice:      m.OnDemandPrice,
			PriceHistory:       priceHistory,
			CPUUsage:           m.CPUUsagePercent / 100.0,
			MemoryUsage:        m.MemoryUsagePercent / 100.0,
			ClusterUtilization: clusterUtil,
			IsSpot:             m.IsSpot,
			Timestamp:          time.Now(),
			// REAL TELEMETRY (Phase 4)
			PodStartupTime:     poolFeats.PodStartupTime,
			OutagePenaltyHours: poolFeats.OutagePenaltyHours,
			MigrationCost:      migCost,
			TimeSinceMigration: tsm,
			CurrentSpotRatio:   currentRatio,
			TargetSpotRatio:    targetRatio,
			PriorityScore:      poolFeats.PriorityScore,
		}

		action, capacityScore, runtimeScore, confidence, err := c.inf.PredictDetailed(ctx, m.NodeID, state, riskMult)

		if err != nil {
			c.logger.Error("inference failed for node", "node_id", m.NodeID, "error", err)
			continue // Skip node, but don't fail entire loop
		}

		decisionSource := "rl"
		if useDeterministic {
			deterministicAction, deterministic := evaluateDeterministicPolicy(state, float64(capacityScore), float64(runtimeScore), runtimeCfg)
			action = deterministicAction
			confidence = 1.0
			decisionSource = "deterministic"

			metrics.DeterministicDecisionReason.WithLabelValues(deterministic.Reason).Inc()
			metrics.WorkloadCap.WithLabelValues(poolID).Set(deterministic.EffectiveCap)
			if deterministic.IsOOD {
				metrics.WorkloadOOD.WithLabelValues(poolID).Set(1.0)
				for _, reason := range deterministic.OODReasons {
					metrics.WorkloadOODReason.WithLabelValues(reason).Inc()
				}
			} else {
				metrics.WorkloadOOD.WithLabelValues(poolID).Set(0.0)
			}
		}
		metrics.DecisionSource.WithLabelValues(decisionSource, inference.ActionToString(action)).Inc()

		assessments = append(assessments, NodeAssessment{
			NodeID:        m.NodeID,
			Action:        action,
			CapacityScore: capacityScore,
			RuntimeScore:  runtimeScore,
			Confidence:    confidence,
		})

		metrics.CapacityScore.WithLabelValues(m.NodeID, m.Zone).Set(float64(capacityScore))
		metrics.RuntimeScore.WithLabelValues(poolID, m.Zone).Set(float64(runtimeScore))
		if m.InstanceType != "" && m.Zone != "" {
			metrics.SpotPriceUSD.WithLabelValues(m.InstanceType, m.Zone).Set(m.SpotPrice)
		}
		if m.InstanceType != "" {
			metrics.OnDemandPriceUSD.WithLabelValues(m.InstanceType).Set(m.OnDemandPrice)
		}
	}

	return assessments, nil
}

func (c *Controller) calculateClusterUtilization(metrics []metrics.NodeMetrics) float64 {
	if len(metrics) == 0 {
		return 0
	}
	var total float64
	for _, m := range metrics {
		total += m.CPUUsagePercent
	}
	return (total / float64(len(metrics))) / 100.0
}

func normalizePriceHistory(history []float64, currentPrice float64) []float64 {
	if len(history) == 0 {
		if currentPrice > 0 {
			return []float64{currentPrice}
		}
		return history
	}

	if len(history) >= inference.TFTHistorySteps*2 {
		recent := history[len(history)-inference.TFTHistorySteps*2:]
		downsampled := make([]float64, 0, inference.TFTHistorySteps)
		for i := 1; i < len(recent); i += 2 {
			downsampled = append(downsampled, recent[i])
		}
		if len(downsampled) == inference.TFTHistorySteps {
			return downsampled
		}
	}

	if len(history) > inference.TFTHistorySteps {
		return history[len(history)-inference.TFTHistorySteps:]
	}

	return history
}

// poolAggregation holds aggregated data for per-pool inference.
type poolAggregation struct {
	nodes        []metrics.NodeMetrics
	dominantType string // Most common instance type by count
	typeCounts   map[string]int
	avgCPUUsage  float64
	avgMemUsage  float64
	spotNodes    int
	odNodes      int
	workloadPool string
	zone         string
}

// runPoolLevelInference implements Section 6 Option 2 of PRODUCTION_FLOW_EKS_KARPENTER.md.
// It aggregates market telemetry to pool-level and runs inference once per pool using
// the dominant instance type (by count). This provides "one coherent action per pool per tick".
func (c *Controller) runPoolLevelInference(ctx context.Context, nodeMetrics []metrics.NodeMetrics) ([]NodeAssessment, error) {
	runtimeCfg := c.loadRuntimeConfig()
	riskMult := runtimeCfg.RiskMultiplier
	stepMinutes := runtimeCfg.StepMinutes
	useDeterministic := runtimeCfg.UseDeterministicPolicy()

	assessments := make([]NodeAssessment, 0, len(nodeMetrics))

	// Get cluster-wide stats for feature building
	clusterUtil := c.calculateClusterUtilization(nodeMetrics)
	nodeInfo, err := c.nodeInfoMap(ctx)
	if err != nil {
		c.logger.Warn("failed to load node labels", "error", err)
	}

	// Step 1: Group nodes by pool and find dominant instance type
	poolAggregations := make(map[string]*poolAggregation)
	nodeToPoolID := make(map[string]string)

	for _, raw := range nodeMetrics {
		m := raw
		m.NodeID = strings.TrimSpace(m.NodeID)
		info, ok := nodeInfo[m.NodeID]
		if !ok {
			host := strings.Split(m.NodeID, ":")[0]
			info, ok = nodeInfo[host]
		}
		if ok {
			m.NodeID = info.name
			if m.Zone == "" {
				m.Zone = info.zone
			}
			if m.InstanceType == "" {
				m.InstanceType = info.instanceType
			}
			m.IsSpot = info.isSpot
		}

		// For pool-level inference, we group by workload pool + zone (not instance type)
		// This allows "one action per pool" regardless of instance type mix
		workloadPool := ""
		if info, ok := nodeInfo[m.NodeID]; ok {
			workloadPool = info.workloadPool
		}

		// Pool key for aggregation: workloadPool:zone (or just zone if no workload pool)
		poolKey := m.Zone
		if workloadPool != "" {
			poolKey = workloadPool + ":" + m.Zone
		}
		if m.Zone == "" {
			poolKey = "unknown"
		}

		agg, exists := poolAggregations[poolKey]
		if !exists {
			agg = &poolAggregation{
				nodes:        make([]metrics.NodeMetrics, 0),
				typeCounts:   make(map[string]int),
				workloadPool: workloadPool,
				zone:         m.Zone,
			}
			poolAggregations[poolKey] = agg
		}

		agg.nodes = append(agg.nodes, m)
		agg.typeCounts[m.InstanceType]++
		agg.avgCPUUsage += m.CPUUsagePercent
		agg.avgMemUsage += m.MemoryUsagePercent
		if m.IsSpot {
			agg.spotNodes++
		} else {
			agg.odNodes++
		}

		nodeToPoolID[m.NodeID] = poolKey
	}

	// Finalize aggregations and find dominant instance type
	for poolKey, agg := range poolAggregations {
		if len(agg.nodes) > 0 {
			agg.avgCPUUsage /= float64(len(agg.nodes))
			agg.avgMemUsage /= float64(len(agg.nodes))
		}

		// Find dominant instance type (most common by count)
		maxCount := 0
		for instType, count := range agg.typeCounts {
			if count > maxCount {
				maxCount = count
				agg.dominantType = instType
			}
		}

		c.logger.Debug("pool aggregation",
			"pool_key", poolKey,
			"dominant_type", agg.dominantType,
			"node_count", len(agg.nodes),
			"spot_nodes", agg.spotNodes,
			"od_nodes", agg.odNodes,
		)
	}

	// Step 2: Update pool counts and ratios
	poolCounts := make(map[string]*poolCount)
	for poolKey, agg := range poolAggregations {
		poolCounts[poolKey] = &poolCount{
			total: len(agg.nodes),
			spot:  agg.spotNodes,
		}
	}

	currentSpotRatio := make(map[string]float64, len(poolCounts))
	for poolID, counts := range poolCounts {
		if counts.total > 0 {
			currentSpotRatio[poolID] = float64(counts.spot) / float64(counts.total)
		}
	}

	targetSpotRatio := make(map[string]float64, len(poolCounts))
	c.historyLock.Lock()
	c.poolNodeCounts = poolCounts
	c.currentSpotRatio = currentSpotRatio
	for poolID, ratio := range currentSpotRatio {
		if _, ok := c.targetSpotRatio[poolID]; !ok {
			c.targetSpotRatio[poolID] = ratio
		}
	}
	for poolID, ratio := range c.targetSpotRatio {
		targetSpotRatio[poolID] = ratio
	}
	c.historyLock.Unlock()

	// Step 3: Run inference once per pool using dominant instance type
	poolActions := make(map[string]NodeAssessment)
	priceCache := make(map[string]cloudapi.SpotPriceData)

	for poolKey, agg := range poolAggregations {
		if len(agg.nodes) == 0 {
			continue
		}

		// Use dominant instance type for price lookup
		instanceType := agg.dominantType
		zone := agg.zone
		if instanceType == "" || zone == "" {
			continue
		}

		if supported, reason := c.inf.SupportsInstanceType(instanceType); !supported {
			poolActions[poolKey] = c.unsupportedFamilyAssessment(poolKey, instanceType, reason)
			continue
		}

		// Build a representative poolID for this aggregation
		poolID := c.getPoolIDWithExtendedFormat(instanceType, zone, agg.workloadPool)

		// Get price data for the dominant instance type
		var priceHistory []float64
		var spotPrice, odPrice float64

		if c.price != nil {
			// Synthetic mode - use first node for pricing
			spotPrice, odPrice, priceHistory = c.price.Next(agg.nodes[0].NodeID, agg.spotNodes > 0)
		} else if c.priceP != nil {
			cacheKey := instanceType + ":" + zone
			if cached, ok := priceCache[cacheKey]; ok {
				spotPrice = cached.CurrentPrice
				odPrice = cached.OnDemandPrice
				priceHistory = append([]float64(nil), cached.PriceHistory...)
			} else {
				data, err := c.priceP.GetSpotPrice(ctx, instanceType, zone)
				if err != nil {
					c.logger.Warn("failed to fetch spot price data for pool",
						"pool", poolKey,
						"instance_type", instanceType,
						"error", err,
					)
				} else {
					priceCache[cacheKey] = data
					spotPrice = data.CurrentPrice
					odPrice = data.OnDemandPrice
					priceHistory = append([]float64(nil), data.PriceHistory...)
				}
			}
		}

		// Fall back to price history from controller state
		if len(priceHistory) == 0 {
			c.historyLock.Lock()
			hist := c.priceHistory[poolID]
			if spotPrice > 0 {
				hist = append(hist, spotPrice)
				if len(hist) > inference.TFTHistorySteps*2 {
					hist = hist[len(hist)-inference.TFTHistorySteps*2:]
				}
				c.priceHistory[poolID] = hist
			}
			priceHistory = make([]float64, len(hist))
			copy(priceHistory, hist)
			c.historyLock.Unlock()
		}

		priceHistory = normalizePriceHistory(priceHistory, spotPrice)
		if spotPrice <= 0 || odPrice <= 0 || len(priceHistory) == 0 {
			c.logger.Warn("skipping pool inference due to missing market telemetry",
				"pool", poolKey,
				"instance_type", instanceType,
				"zone", zone,
				"spot_price", spotPrice,
				"ondemand_price", odPrice,
				"history_points", len(priceHistory),
			)
			continue
		}

		poolFeats := c.coll.GetPoolFeatures(poolID)

		// Calculate Migration Cost using pool-level averages
		startupMins := poolFeats.PodStartupTime / 60.0
		hourlyDelta := maxFloat(0.0, odPrice-spotPrice)
		migCost := (startupMins / 60.0) * hourlyDelta * clusterUtil * 2.0

		// TimeSinceMigration
		tsm := 100
		c.historyLock.Lock()
		if last, ok := c.lastMigration[poolID]; ok {
			tsm = stepsSinceMigration(last, stepMinutes)
		}
		c.historyLock.Unlock()

		// Build pool-level state (using averaged metrics)
		state := inference.NodeState{
			SpotPrice:          spotPrice,
			OnDemandPrice:      odPrice,
			PriceHistory:       priceHistory,
			CPUUsage:           agg.avgCPUUsage / 100.0,
			MemoryUsage:        agg.avgMemUsage / 100.0,
			ClusterUtilization: clusterUtil,
			IsSpot:             agg.spotNodes > agg.odNodes, // Majority determines
			Timestamp:          time.Now(),
			PodStartupTime:     poolFeats.PodStartupTime,
			OutagePenaltyHours: poolFeats.OutagePenaltyHours,
			MigrationCost:      migCost,
			TimeSinceMigration: tsm,
			CurrentSpotRatio:   currentSpotRatio[poolKey],
			TargetSpotRatio:    targetSpotRatio[poolKey],
			PriorityScore:      poolFeats.PriorityScore,
		}

		// Run inference once for this pool
		action, capacityScore, runtimeScore, confidence, err := c.inf.PredictDetailed(ctx, poolKey, state, riskMult)
		if err != nil {
			c.logger.Error("pool-level inference failed", "pool", poolKey, "error", err)
			continue
		}

		decisionSource := "rl"
		if useDeterministic {
			deterministicAction, deterministic := evaluateDeterministicPolicy(state, float64(capacityScore), float64(runtimeScore), runtimeCfg)
			action = deterministicAction
			confidence = 1.0
			decisionSource = "deterministic"

			metrics.DeterministicDecisionReason.WithLabelValues(deterministic.Reason).Inc()
			metrics.WorkloadCap.WithLabelValues(poolKey).Set(deterministic.EffectiveCap)
			if deterministic.IsOOD {
				metrics.WorkloadOOD.WithLabelValues(poolKey).Set(1.0)
				for _, reason := range deterministic.OODReasons {
					metrics.WorkloadOODReason.WithLabelValues(reason).Inc()
				}
			} else {
				metrics.WorkloadOOD.WithLabelValues(poolKey).Set(0.0)
			}
		}
		metrics.DecisionSource.WithLabelValues(decisionSource, inference.ActionToString(action)).Inc()

		c.logger.Info("pool-level inference complete",
			"pool", poolKey,
			"dominant_type", instanceType,
			"node_count", len(agg.nodes),
			"action", inference.ActionToString(action),
			"capacity_score", capacityScore,
			"runtime_score", runtimeScore,
			"confidence", confidence,
		)

		poolActions[poolKey] = NodeAssessment{
			NodeID:        poolKey, // Pool-level action
			Action:        action,
			CapacityScore: capacityScore,
			RuntimeScore:  runtimeScore,
			Confidence:    confidence,
		}

		// Emit metrics for the pool
		metrics.CapacityScore.WithLabelValues(poolKey, zone).Set(float64(capacityScore))
		metrics.RuntimeScore.WithLabelValues(poolKey, zone).Set(float64(runtimeScore))
		if instanceType != "" && zone != "" {
			metrics.SpotPriceUSD.WithLabelValues(instanceType, zone).Set(spotPrice)
		}
		if instanceType != "" {
			metrics.OnDemandPriceUSD.WithLabelValues(instanceType).Set(odPrice)
		}
	}

	// Step 4: Apply pool-level action to all nodes in each pool
	for _, m := range nodeMetrics {
		nodeID := strings.TrimSpace(m.NodeID)
		instanceType := m.InstanceType
		if info, ok := nodeInfo[nodeID]; ok {
			nodeID = info.name
			if instanceType == "" {
				instanceType = info.instanceType
			}
		}

		poolKey := nodeToPoolID[nodeID]
		if poolKey == "" {
			continue
		}

		poolAction, ok := poolActions[poolKey]
		if !ok {
			continue
		}

		if supported, reason := c.inf.SupportsInstanceType(instanceType); !supported {
			assessments = append(assessments, c.unsupportedFamilyAssessment(nodeID, instanceType, reason))
			continue
		}

		// Apply the same action to all nodes in the pool
		assessments = append(assessments, NodeAssessment{
			NodeID:        nodeID,
			Action:        poolAction.Action,
			CapacityScore: poolAction.CapacityScore,
			RuntimeScore:  poolAction.RuntimeScore,
			Confidence:    poolAction.Confidence,
		})
	}

	return assessments, nil
}

// generateAndLogSavingsReport builds and logs a savings report for dry-run mode.
// This shows customers the potential value of SpotVortex before enabling active management.
func (c *Controller) generateAndLogSavingsReport(ctx context.Context, nodeMetrics []metrics.NodeMetrics, assessments []NodeAssessment) {
	// Build node info map
	nodeInfo, _ := c.nodeInfoMap(ctx)

	// Build assessment map for quick lookup
	assessmentMap := make(map[string]NodeAssessment)
	for _, a := range assessments {
		assessmentMap[a.NodeID] = a
	}

	// Group nodes by pool and collect pricing info
	poolAggregations := make(map[string]*poolAggregation)
	poolActions := make(map[string]NodeAssessment)
	priceCache := make(map[string]float64)

	for _, m := range nodeMetrics {
		nodeID := strings.TrimSpace(m.NodeID)
		info, ok := nodeInfo[nodeID]
		if !ok {
			host := strings.Split(nodeID, ":")[0]
			info, ok = nodeInfo[host]
		}

		instanceType := m.InstanceType
		zone := m.Zone
		isSpot := m.IsSpot
		workloadPool := ""

		if ok {
			nodeID = info.name
			if instanceType == "" {
				instanceType = info.instanceType
			}
			if zone == "" {
				zone = info.zone
			}
			isSpot = info.isSpot
			workloadPool = info.workloadPool
		}

		// Pool key
		poolKey := zone
		if workloadPool != "" {
			poolKey = workloadPool + ":" + zone
		}
		if zone == "" {
			poolKey = "unknown"
		}

		agg, exists := poolAggregations[poolKey]
		if !exists {
			agg = &poolAggregation{
				nodes:        make([]metrics.NodeMetrics, 0),
				typeCounts:   make(map[string]int),
				workloadPool: workloadPool,
				zone:         zone,
			}
			poolAggregations[poolKey] = agg
		}

		// Store node with enriched info
		enrichedNode := m
		enrichedNode.NodeID = nodeID
		enrichedNode.InstanceType = instanceType
		enrichedNode.Zone = zone
		enrichedNode.IsSpot = isSpot
		agg.nodes = append(agg.nodes, enrichedNode)
		agg.typeCounts[instanceType]++
		if isSpot {
			agg.spotNodes++
		} else {
			agg.odNodes++
		}

		// Find dominant type
		maxCount := 0
		for it, count := range agg.typeCounts {
			if count > maxCount {
				maxCount = count
				agg.dominantType = it
			}
		}

		// Store pool action from assessment
		if assessment, found := assessmentMap[nodeID]; found {
			poolActions[poolKey] = assessment
		}

		// Fetch and cache prices
		cacheKey := instanceType + ":" + zone
		if _, hasCached := priceCache[cacheKey+":spot"]; !hasCached && c.priceP != nil {
			if data, err := c.priceP.GetSpotPrice(ctx, instanceType, zone); err == nil {
				priceCache[cacheKey+":spot"] = data.CurrentPrice
				priceCache[cacheKey+":od"] = data.OnDemandPrice
			}
		}
	}

	// Build the savings report
	report := c.buildSavingsReport(ctx, poolAggregations, poolActions, priceCache)

	// Emit metrics for Grafana dashboards
	emitDryRunMetrics(report)

	// Log the report in customer-friendly format
	logDryRunReport(c.logger, report)
}

// applyTargetSpotRatioWithConfig updates the target spot ratio based on RL action,
// clamping to runtime config bounds. If riskLow is true (TFT score below threshold),
// gently lerps toward the configured target.
func (c *Controller) applyTargetSpotRatioWithConfig(poolID string, action inference.Action, runtimeCfg *config.RuntimeConfig, riskLow bool) {
	if poolID == "" {
		return
	}

	delta := 0.0
	setValue := -1.0
	switch action {
	case inference.ActionHold:
		// HOLD allows riskLow drift toward runtime target.
		// Keep delta=0 and apply optional lerp below.
	case inference.ActionIncrease10:
		delta = 0.10
	case inference.ActionIncrease30:
		delta = 0.30
	case inference.ActionDecrease10:
		delta = -0.10
	case inference.ActionDecrease30:
		delta = -0.30
	case inference.ActionEmergencyExit:
		setValue = 0.0
	default:
		return
	}

	c.historyLock.Lock()
	defer c.historyLock.Unlock()

	current := c.targetSpotRatio[poolID]
	var updated float64

	if setValue >= 0 {
		updated = setValue
	} else {
		updated = current + delta
	}

	// Basic 0-1 clamping
	if updated < 0 {
		updated = 0
	} else if updated > 1 {
		updated = 1
	}

	// Apply runtime config bounds (min/max spot ratio)
	if runtimeCfg != nil {
		updated = runtimeCfg.ClampSpotRatio(updated)

		// Optional: lerp toward target_spot_ratio when market is safe (low risk)
		// Alpha = 0.1 for gradual drift (per Section 5.1.1 of production doc)
		if riskLow && action == inference.ActionHold {
			// Only lerp on HOLD actions when market is calm
			updated = lerp(updated, runtimeCfg.TargetSpotRatio, 0.1)
			updated = runtimeCfg.ClampSpotRatio(updated) // re-clamp after lerp
		}
	}

	c.targetSpotRatio[poolID] = updated
}

// applyTargetSpotRatio is the backward-compatible version without runtime config.
// Deprecated: Use applyTargetSpotRatioWithConfig for production.
func (c *Controller) applyTargetSpotRatio(poolID string, action inference.Action) {
	c.applyTargetSpotRatioWithConfig(poolID, action, nil, false)
}

// lerp performs linear interpolation between a and b.
func lerp(a, b, t float64) float64 {
	return a + t*(b-a)
}

// calculatePoolDrainCount calculates how many nodes to drain for a pool to reach target ratio.
// Per PRODUCTION_FLOW_EKS_KARPENTER.md Section 5.4:
//
//	need = ceil((r_tgt - r_cur) * total_nodes)
//	drains = min(need, MaxDrainsPerTick, ...)
func (c *Controller) calculatePoolDrainCount(poolID string, action inference.Action) int {
	c.historyLock.Lock()
	counts, ok := c.poolNodeCounts[poolID]
	currentRatio := c.currentSpotRatio[poolID]
	targetRatio := c.targetSpotRatio[poolID]
	c.historyLock.Unlock()

	if !ok || counts.total == 0 {
		return 0
	}

	// Calculate delta based on action
	delta := 0.0
	switch action {
	case inference.ActionIncrease10:
		delta = 0.10
	case inference.ActionIncrease30:
		delta = 0.30
	case inference.ActionDecrease10:
		delta = -0.10
	case inference.ActionDecrease30:
		delta = -0.30
	case inference.ActionEmergencyExit:
		// Emergency exit: target = 0, so need to drain all spot nodes
		return counts.spot
	default:
		return 0
	}

	// Calculate needed drains
	newTargetRatio := targetRatio + delta
	if newTargetRatio < 0 {
		newTargetRatio = 0
	} else if newTargetRatio > 1 {
		newTargetRatio = 1
	}

	ratioDiff := newTargetRatio - currentRatio
	need := int(math.Ceil(math.Abs(ratioDiff) * float64(counts.total)))

	// Apply rate limit (maxDrainRatio of total cluster per tick)
	maxDrain := int(float64(counts.total) * c.maxDrainRatio)
	if maxDrain < 1 {
		maxDrain = 1
	}

	if need > maxDrain {
		c.logger.Info("pool drain rate limited",
			"pool", poolID,
			"needed", need,
			"max_allowed", maxDrain,
		)
		return maxDrain
	}

	return need
}

// steerKarpenterWeights updates Karpenter NodePool weights based on target spot ratio.
// Per PRODUCTION_FLOW_EKS_KARPENTER.md Section 5.4:
// - Higher spot weight when we want more spot (INCREASE actions)
// - Higher on-demand weight when we want less spot (DECREASE actions)
// Per Section 2.4: Only touches objects it owns (allowlist) and patches weights slowly (cooldown).
func (c *Controller) steerKarpenterWeights(ctx context.Context, workloadPool string, favorSpot bool) error {
	if c.nodePoolMgr == nil || !c.karpenterCfg.Enabled {
		return nil
	}

	// Check allowlist: only manage pools explicitly listed (or all if list is empty)
	if !c.karpenterCfg.IsWorkloadPoolManaged(workloadPool) {
		c.logger.Info("skipping weight steering for unmanaged workload pool",
			"workload_pool", workloadPool,
		)
		return nil
	}

	// Check cooldown: don't change weights too frequently (hysteresis)
	c.historyLock.Lock()
	lastChange, hasLastChange := c.lastWeightChange[workloadPool]
	c.historyLock.Unlock()

	cooldown := c.karpenterCfg.WeightChangeCooldown()
	if hasLastChange && time.Since(lastChange) < cooldown {
		c.logger.Info("skipping weight steering due to cooldown",
			"workload_pool", workloadPool,
			"last_change", lastChange,
			"cooldown", cooldown,
			"remaining", cooldown-time.Since(lastChange),
		)
		return nil
	}

	spotPoolName := workloadPool + c.karpenterCfg.SpotNodePoolSuffix
	odPoolName := workloadPool + c.karpenterCfg.OnDemandNodePoolSuffix

	var spotWeight, odWeight int32
	if favorSpot {
		// Favor spot: give spot NodePool higher weight
		spotWeight = c.karpenterCfg.SpotWeight
		odWeight = c.karpenterCfg.OnDemandWeight
	} else {
		// Favor on-demand: give on-demand NodePool higher weight
		spotWeight = c.karpenterCfg.OnDemandWeight
		odWeight = c.karpenterCfg.SpotWeight
	}

	c.logger.Info("steering Karpenter weights",
		"workload_pool", workloadPool,
		"spot_nodepool", spotPoolName,
		"od_nodepool", odPoolName,
		"favor_spot", favorSpot,
		"spot_weight", spotWeight,
		"od_weight", odWeight,
	)

	// Update spot NodePool weight
	spotErr := c.nodePoolMgr.SetWeight(ctx, spotPoolName, spotWeight)
	if spotErr != nil {
		c.logger.Warn("failed to set spot NodePool weight",
			"nodepool", spotPoolName,
			"error", spotErr,
		)
	}

	// Update on-demand NodePool weight
	odErr := c.nodePoolMgr.SetWeight(ctx, odPoolName, odWeight)
	if odErr != nil {
		c.logger.Warn("failed to set on-demand NodePool weight",
			"nodepool", odPoolName,
			"error", odErr,
		)
	}

	// Record weight change time if at least one succeeded
	if spotErr == nil || odErr == nil {
		c.historyLock.Lock()
		c.lastWeightChange[workloadPool] = time.Now()
		c.historyLock.Unlock()
	}

	return nil
}

// batchSteerKarpenterWeights aggregates weight steering decisions per workload pool
// and applies weights once per pool before any drains occur.
// This ensures:
// 1. Weights are updated BEFORE drains (so Karpenter provisions correct capacity type)
// 2. We make at most one weight change per pool per reconcile cycle
// 3. We respect the allowlist and cooldown constraints
func (c *Controller) batchSteerKarpenterWeights(ctx context.Context, nodes []NodeAssessment) {
	if c.nodePoolMgr == nil || !c.karpenterCfg.Enabled {
		return
	}

	// Aggregate decisions per workload pool
	// Map: workloadPool -> {favorSpotCount, favorODCount}
	type poolDecision struct {
		favorSpot int
		favorOD   int
	}
	poolDecisions := make(map[string]*poolDecision)

	for _, node := range nodes {
		// We need to get the workload pool for this node
		// This requires looking up node labels
		if c.k8s == nil {
			continue
		}
		nodeObj, err := c.k8s.CoreV1().Nodes().Get(ctx, node.NodeID, metav1.GetOptions{})
		if err != nil {
			continue
		}
		labels := nodeObj.Labels
		if labels == nil {
			continue
		}
		workloadPool := labels[collector.WorkloadPoolLabel]
		if workloadPool == "" {
			continue
		}

		decision, ok := poolDecisions[workloadPool]
		if !ok {
			decision = &poolDecision{}
			poolDecisions[workloadPool] = decision
		}

		// Count decisions per pool
		switch node.Action {
		case inference.ActionIncrease10, inference.ActionIncrease30:
			decision.favorSpot++
		case inference.ActionDecrease10, inference.ActionDecrease30, inference.ActionEmergencyExit:
			decision.favorOD++
		}
	}

	// Apply weights once per pool based on majority decision
	for workloadPool, decision := range poolDecisions {
		// If more nodes need spot, favor spot; otherwise favor on-demand
		// Ties go to on-demand (safer)
		favorSpot := decision.favorSpot > decision.favorOD

		c.logger.Info("batch steering weights for pool",
			"workload_pool", workloadPool,
			"favor_spot_votes", decision.favorSpot,
			"favor_od_votes", decision.favorOD,
			"result", map[bool]string{true: "favor_spot", false: "favor_on_demand"}[favorSpot],
		)

		if err := c.steerKarpenterWeights(ctx, workloadPool, favorSpot); err != nil {
			c.logger.Warn("failed to steer weights for pool", "workload_pool", workloadPool, "error", err)
		}
	}
}

// getWorkloadPoolFromPoolID extracts the workload pool name from a pool ID.
// Pool ID formats:
// - Simple: "<instance_type>:<zone>" -> returns empty string (no workload pool)
// - Extended: "<workload_pool>:<instance_type>:<zone>" -> returns workload_pool
func getWorkloadPoolFromPoolID(poolID string) string {
	parts := strings.Split(poolID, ":")
	if len(parts) == 3 {
		return parts[0]
	}
	return ""
}

// getPoolIDWithExtendedFormat generates pool ID based on configuration.
// If UseExtendedPoolID is enabled, uses format: "<workload_pool>:<instance_type>:<zone>"
// Otherwise uses simple format: "<instance_type>:<zone>"
func (c *Controller) getPoolIDWithExtendedFormat(instanceType, zone, workloadPool string) string {
	if c.karpenterCfg.UseExtendedPoolID && workloadPool != "" {
		return fmt.Sprintf("%s:%s:%s", workloadPool, instanceType, zone)
	}
	return fmt.Sprintf("%s:%s", instanceType, zone)
}

// filterActionableNodes filters assessments to find nodes that need action.
func (c *Controller) filterActionableNodes(assessments []NodeAssessment) []NodeAssessment {
	var actionable []NodeAssessment

	for _, a := range assessments {
		if float64(a.Confidence) < c.confidenceThreshold {
			c.logger.Warn("low confidence, skipping action",
				"node_id", a.NodeID,
				"confidence", a.Confidence,
				"threshold", c.confidenceThreshold,
			)
			continue
		}

		// RL Decision taking precedence
		// Prime Directive Safety check on Capacity Score (TFT fallback)
		// Always override to EMERGENCY_EXIT if risk is above threshold,
		// regardless of the RL action. This keeps production safety aligned
		// with evaluation guardrails.
		if a.CapacityScore > float32(c.riskThreshold) {
			c.logger.Warn("TFT risk threshold exceeded (Prime Directive)",
				"node_id", a.NodeID,
				"capacity_score", a.CapacityScore,
				"action", "FORCED_MIGRATE",
			)
			a.Action = inference.ActionEmergencyExit // Upgrade to immediate drain
			actionable = append(actionable, a)
			continue
		}

		if a.Action != inference.ActionHold {
			c.logger.Warn("RL recommends action",
				"node_id", a.NodeID,
				"action", inference.ActionToString(a.Action),
				"capacity_score", a.CapacityScore,
			)
			actionable = append(actionable, a)
			continue
		}
	}

	return actionable
}

func (c *Controller) filterExecutableNodes(ctx context.Context, assessments []NodeAssessment) []NodeAssessment {
	info, err := c.nodeInfoMap(ctx)
	if err != nil {
		c.logger.Warn("failed to load node metadata for action filtering", "error", err)
		return assessments
	}

	filtered := make([]NodeAssessment, 0, len(assessments))
	for _, a := range assessments {
		meta, ok := info[a.NodeID]
		if ok {
			if !meta.isManaged {
				c.logger.Info("skipping unmanaged node", "node_id", a.NodeID)
				continue
			}
			if meta.isControl {
				c.logger.Info("skipping control-plane node", "node_id", a.NodeID)
				continue
			}
			if meta.isFake {
				c.logger.Info("skipping fake node action", "node_id", a.NodeID)
				continue
			}
			switch a.Action {
			case inference.ActionIncrease10, inference.ActionIncrease30:
				if meta.isSpot {
					c.logger.Info("skipping increase action on spot node", "node_id", a.NodeID)
					continue
				}
			case inference.ActionDecrease10, inference.ActionDecrease30, inference.ActionEmergencyExit:
				if !meta.isSpot {
					c.logger.Info("skipping decrease action on on-demand node", "node_id", a.NodeID)
					continue
				}
			default:
			}
		}
		filtered = append(filtered, a)
	}

	return filtered
}

// applyDrainLimit enforces thundering herd protection with pool-level awareness.
// Per PRODUCTION_FLOW_EKS_KARPENTER.md Section 5.4: limit drains per pool based on target ratio delta.
// Per Section 2.4: "keep drain concurrency below budgets" - respects Karpenter disruption budgets.
func (c *Controller) applyDrainLimit(ctx context.Context, nodes []NodeAssessment, totalNodes int) []NodeAssessment {
	// Global max drain limit from config
	maxDrain := int(float64(totalNodes) * c.maxDrainRatio)
	if maxDrain < 1 {
		maxDrain = 1
	}

	if len(nodes) == 0 {
		return nodes
	}

	// Sort nodes by priority: EMERGENCY_EXIT first, then by CapacityScore (descending)
	sortedNodes := make([]NodeAssessment, len(nodes))
	copy(sortedNodes, nodes)
	for i := 0; i < len(sortedNodes)-1; i++ {
		for j := i + 1; j < len(sortedNodes); j++ {
			// EMERGENCY_EXIT actions take priority
			iEmergency := sortedNodes[i].Action == inference.ActionEmergencyExit
			jEmergency := sortedNodes[j].Action == inference.ActionEmergencyExit
			if jEmergency && !iEmergency {
				sortedNodes[i], sortedNodes[j] = sortedNodes[j], sortedNodes[i]
			} else if iEmergency == jEmergency && sortedNodes[j].CapacityScore > sortedNodes[i].CapacityScore {
				sortedNodes[i], sortedNodes[j] = sortedNodes[j], sortedNodes[i]
			}
		}
	}

	// Apply Karpenter disruption budget awareness (per Section 2.4)
	// "NodePool disruption budgets rate-limit Karpenter's voluntary disruption,
	// and they count nodes being deleted for any reason (including your drains)"
	if c.nodePoolMgr != nil && c.karpenterCfg.Enabled {
		karpenterLimit := c.getKarpenterDisruptionLimit(ctx, sortedNodes, totalNodes)
		if karpenterLimit >= 0 && karpenterLimit < maxDrain {
			c.logger.Info("applying Karpenter disruption budget limit",
				"config_limit", maxDrain,
				"karpenter_limit", karpenterLimit,
			)
			maxDrain = karpenterLimit
		}
	}

	// Apply global limit
	if len(sortedNodes) > maxDrain {
		c.logger.Warn("applying drain limit (thundering herd protection)",
			"at_risk", len(sortedNodes),
			"max_drain", maxDrain,
			"deferred", len(sortedNodes)-maxDrain,
		)
		sortedNodes = sortedNodes[:maxDrain]
	}

	return sortedNodes
}

// getKarpenterDisruptionLimit calculates the effective drain limit based on
// Karpenter NodePool disruption budgets.
// Returns the minimum limit across all relevant NodePools, or -1 if no limit.
func (c *Controller) getKarpenterDisruptionLimit(ctx context.Context, nodes []NodeAssessment, totalNodes int) int {
	if c.nodePoolMgr == nil || c.k8s == nil {
		return -1
	}

	// Group nodes by workload pool to check their respective NodePool budgets
	workloadPools := make(map[string]int) // workloadPool -> node count
	for _, node := range nodes {
		nodeObj, err := c.k8s.CoreV1().Nodes().Get(ctx, node.NodeID, metav1.GetOptions{})
		if err != nil {
			continue
		}
		labels := nodeObj.Labels
		if labels == nil {
			continue
		}
		workloadPool := labels[collector.WorkloadPoolLabel]
		if workloadPool != "" {
			workloadPools[workloadPool]++
		}
	}

	if len(workloadPools) == 0 {
		return -1
	}

	// Check disruption budgets for both spot and on-demand NodePools
	minLimit := -1
	for workloadPool := range workloadPools {
		// Check spot NodePool budget
		spotPoolName := workloadPool + c.karpenterCfg.SpotNodePoolSuffix
		spotLimit, err := c.nodePoolMgr.GetEffectiveDisruptionLimit(ctx, spotPoolName, totalNodes)
		if err == nil && spotLimit >= 0 {
			if minLimit < 0 || spotLimit < minLimit {
				minLimit = spotLimit
				c.logger.Debug("found disruption budget limit",
					"nodepool", spotPoolName,
					"limit", spotLimit,
				)
			}
		}

		// Check on-demand NodePool budget
		odPoolName := workloadPool + c.karpenterCfg.OnDemandNodePoolSuffix
		odLimit, err := c.nodePoolMgr.GetEffectiveDisruptionLimit(ctx, odPoolName, totalNodes)
		if err == nil && odLimit >= 0 {
			if minLimit < 0 || odLimit < minLimit {
				minLimit = odLimit
				c.logger.Debug("found disruption budget limit",
					"nodepool", odPoolName,
					"limit", odLimit,
				)
			}
		}
	}

	return minLimit
}

func (c *Controller) nodeHasNamespace(ctx context.Context, nodeName, namespace string) bool {
	if c.k8s == nil {
		return false
	}
	pods, err := c.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		c.logger.Warn("failed to list pods for node", "node_id", nodeName, "error", err)
		return false
	}
	for _, pod := range pods.Items {
		if pod.Namespace != namespace {
			continue
		}
		if isDaemonSetPod(pod.OwnerReferences) {
			continue
		}
		if pod.Namespace == namespace {
			return true
		}
	}
	return false
}

func isDaemonSetPod(owners []metav1.OwnerReference) bool {
	for _, owner := range owners {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// executeAction executes the RL decision on the target node.
func (c *Controller) executeAction(ctx context.Context, node NodeAssessment) error {
	if c.k8s == nil {
		return fmt.Errorf("k8s client required for action execution")
	}

	nodeObj, err := c.k8s.CoreV1().Nodes().Get(ctx, node.NodeID, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch node %s: %w", node.NodeID, err)
	}

	labels := nodeObj.Labels
	if labels == nil || labels["spotvortex.io/managed"] != "true" {
		c.logger.Info("skipping unmanaged node", "node_id", node.NodeID)
		return nil
	}
	if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
		c.logger.Info("skipping control-plane node", "node_id", node.NodeID)
		return nil
	}
	if _, ok := labels["node-role.kubernetes.io/master"]; ok {
		c.logger.Info("skipping control-plane node", "node_id", node.NodeID)
		return nil
	}
	if nodeObj.Spec.Taints != nil {
		for _, taint := range nodeObj.Spec.Taints {
			if taint.Key == "spotvortex.io/fake" {
				c.logger.Info("skipping fake node", "node_id", node.NodeID)
				return nil
			}
		}
	}
	capacityType := ""
	capacityType = labels["karpenter.sh/capacity-type"]
	isSpot := capacityType == "spot"

	c.logger.Info("executing node action",
		"node_id", node.NodeID,
		"action", inference.ActionToString(node.Action),
		"capacity_score", node.CapacityScore,
	)

	shouldDrain := false
	switch node.Action {
	case inference.ActionHold:
		return nil
	case inference.ActionIncrease10, inference.ActionIncrease30:
		if isSpot {
			c.logger.Info("skipping increase action on spot node", "node_id", node.NodeID)
			return nil
		}
		shouldDrain = true
	case inference.ActionDecrease10, inference.ActionDecrease30, inference.ActionEmergencyExit:
		if !isSpot {
			c.logger.Info("skipping decrease action on on-demand node", "node_id", node.NodeID)
			return nil
		}
		shouldDrain = true
	default:
		return nil
	}

	if !shouldDrain {
		return nil
	}

	// Get pool ID - use extended format if configured
	workloadPool := labels[collector.WorkloadPoolLabel]
	instanceType := labels["node.kubernetes.io/instance-type"]
	zone := labels["topology.kubernetes.io/zone"]
	poolID := c.getPoolIDWithExtendedFormat(instanceType, zone, workloadPool)

	// Load runtime config for ratio bounds
	runtimeCfg := c.loadRuntimeConfig()
	riskLow := node.CapacityScore < float32(c.riskThreshold)*0.5 // Consider low risk if below 50% of threshold

	// Update target spot ratio with runtime config bounds
	c.applyTargetSpotRatioWithConfig(poolID, node.Action, runtimeCfg, riskLow)

	// NOTE: Karpenter weight steering is now handled in batch by batchSteerKarpenterWeights()
	// called in reconcile() BEFORE this function. This ensures:
	// 1. Weights are updated BEFORE any drains start
	// 2. Only one weight update per pool per reconcile cycle (efficiency)
	// 3. Cooldown is respected across the batch

	if c.drain == nil {
		c.logger.Info("no drainer configured, skipping actual drain",
			"node_id", node.NodeID,
		)
		return nil
	}

	allowMonitoringDrain := strings.EqualFold(os.Getenv("SPOTVORTEX_ALLOW_MONITORING_DRAIN"), "true") ||
		os.Getenv("SPOTVORTEX_ALLOW_MONITORING_DRAIN") == "1"
	if !allowMonitoringDrain && c.nodeHasNamespace(ctx, node.NodeID, "monitoring") {
		c.logger.Info("skipping drain for monitoring node", "node_id", node.NodeID)
		return nil
	}

	result, err := c.drain.Drain(ctx, node.NodeID)
	if err != nil {
		return err
	}

	if result.Success {
		c.logger.Info("node drained successfully",
			"node_id", node.NodeID,
			"pods_evicted", result.PodsEvicted,
			"duration", result.Duration,
		)

		// Update Migration Timestamp
		// Reconstruct pool ID from node labels... wait, we don't have them handy here easily.
		// We have node.NodeID. We can look up in nodeInfo again if we pass context?
		// Simpler: Just rely on Reconcile loop refreshing labels next time?
		// But we need to update the map NOW.
		// FIX: Pass PoolID to executeAction or NodeAssessment struct.
		// NodeAssessment doesn't have PoolID.
		// Let's deduce it. (It's rough but MVP).

		// Actually, let's just use historyLock to update map.
		// We need the pool ID.
		// ...
		// We can get node details from Client.
		if nodeObj, err := c.k8s.CoreV1().Nodes().Get(ctx, node.NodeID, metav1.GetOptions{}); err == nil {
			poolID := collector.GetNodePoolID(nodeObj)
			c.historyLock.Lock()
			c.lastMigration[poolID] = time.Now()
			c.historyLock.Unlock()
		}

		metrics.ActionTaken.WithLabelValues(inference.ActionToString(node.Action)).Inc()
		metrics.OutagesAvoided.Inc()
	}

	return nil
}
