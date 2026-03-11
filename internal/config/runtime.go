package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// PolicyModeRL keeps the existing RL-driven action selection path.
	PolicyModeRL = "rl"
	// PolicyModeDeterministic enables TFT-risk + deterministic workload policy.
	PolicyModeDeterministic = "deterministic"

	defaultTargetSpotRatioDriftAlpha = 0.10
)

// PoolSafetyVector is the shared runtime contract for pool-level blast-radius
// signals. The runtime computes and consumes this locally; it is not a model
// input for TFT and it does not imply pod-level actuation.
//
// Field status for Phase 1:
// - live: directly measured or approximated from local cluster state now
// - derived_live: computed from other live fields now
// - default: neutral value used only when pool telemetry is absent
type PoolSafetyVector struct {
	// CriticalServiceSpotConcentration is the share (0..1) of critical-service
	// pods in the pool that currently run on spot nodes.
	// Phase 1 status: live approximation from pod priority or critical markers.
	CriticalServiceSpotConcentration float64 `json:"critical_service_spot_concentration"`

	// MinPDBSlackIfOneNodeLost is the minimum remaining PDB slack after the
	// worst single-node loss in the pool. Negative means a one-node loss would
	// violate at least one matching PDB.
	// Phase 1 status: live.
	MinPDBSlackIfOneNodeLost float64 `json:"min_pdb_slack_if_one_node_lost"`

	// MinPDBSlackIfTwoNodesLost is the minimum remaining PDB slack after the
	// worst two-node loss in the pool. Negative means a two-node loss would
	// violate at least one matching PDB.
	// Phase 1 status: live approximation from the two densest node placements.
	MinPDBSlackIfTwoNodesLost float64 `json:"min_pdb_slack_if_two_nodes_lost"`

	// StatefulPodFraction is the fraction (0..1) of workload pods in the pool
	// owned by StatefulSets.
	// Phase 1 status: live.
	StatefulPodFraction float64 `json:"stateful_pod_fraction"`

	// RestartP95Seconds is the pool-level P95 restart or recovery proxy in
	// seconds. Phase 1 uses startup-to-ready latency as the best live proxy.
	// Phase 1 status: live approximation.
	RestartP95Seconds float64 `json:"restart_p95_seconds"`

	// RecoveryBudgetViolationRisk estimates (0..1) how likely a spot loss is to
	// violate recovery or availability budgets for the pool.
	// Phase 1 status: derived_live heuristic.
	// TODO: replace with direct service-budget telemetry when the runtime has it.
	RecoveryBudgetViolationRisk float64 `json:"recovery_budget_violation_risk"`

	// SpareODHeadroomNodes estimates how many on-demand-equivalent nodes of
	// immediate headroom the pool currently has.
	// Phase 1 status: live approximation from current OD nodes and utilization.
	// TODO: replace with scheduler or allocatable-aware headroom.
	SpareODHeadroomNodes float64 `json:"spare_od_headroom_nodes"`

	// ZoneDiversificationScore measures how well the workload is spread across
	// zones: 0 means single-zone, 0.5 means two zones, 1 means three or more.
	// Phase 1 status: live.
	ZoneDiversificationScore float64 `json:"zone_diversification_score"`

	// EvictablePodFraction is the fraction (0..1) of workload pods in the pool
	// that are presently voluntary-evictable under current PDB state.
	// Phase 1 status: live approximation.
	EvictablePodFraction float64 `json:"evictable_pod_fraction"`

	// SafeMaxSpotRatio is the deterministic policy envelope for the maximum spot
	// ratio considered safe for this pool right now.
	// Phase 1 status: derived_live heuristic.
	SafeMaxSpotRatio float64 `json:"safe_max_spot_ratio"`
}

// DefaultPoolSafetyVector returns the neutral default vector used only when
// runtime pool telemetry is absent.
func DefaultPoolSafetyVector() PoolSafetyVector {
	return PoolSafetyVector{
		CriticalServiceSpotConcentration: 0.0,
		MinPDBSlackIfOneNodeLost:         0.0,
		MinPDBSlackIfTwoNodesLost:        0.0,
		StatefulPodFraction:              0.0,
		RestartP95Seconds:                300.0,
		RecoveryBudgetViolationRisk:      0.0,
		SpareODHeadroomNodes:             0.0,
		ZoneDiversificationScore:         1.0,
		EvictablePodFraction:             1.0,
		SafeMaxSpotRatio:                 1.0,
	}
}

// NormalizePoolSafetyVector clamps pool-safety ratios to sane ranges while
// preserving signed PDB slack values.
func NormalizePoolSafetyVector(v PoolSafetyVector) PoolSafetyVector {
	v.CriticalServiceSpotConcentration = clampFloat(v.CriticalServiceSpotConcentration, 0, 1)
	v.StatefulPodFraction = clampFloat(v.StatefulPodFraction, 0, 1)
	if v.RestartP95Seconds < 0 {
		v.RestartP95Seconds = 0
	}
	v.RecoveryBudgetViolationRisk = clampFloat(v.RecoveryBudgetViolationRisk, 0, 1)
	if v.SpareODHeadroomNodes < 0 {
		v.SpareODHeadroomNodes = 0
	}
	v.ZoneDiversificationScore = clampFloat(v.ZoneDiversificationScore, 0, 1)
	v.EvictablePodFraction = clampFloat(v.EvictablePodFraction, 0, 1)
	v.SafeMaxSpotRatio = clampFloat(v.SafeMaxSpotRatio, 0, 1)
	return v
}

// IsZero reports whether no pool-safety vector has been populated yet.
func (v PoolSafetyVector) IsZero() bool {
	return v == (PoolSafetyVector{})
}

// RuntimeConfig holds dynamic configuration that can be changed without restarting the agent.
// These values are reloaded on every reconcile loop to allow runtime tuning.
type RuntimeConfig struct {
	// RiskMultiplier adjusts the TFT risk score. >1 is more conservative, <1 is more aggressive.
	RiskMultiplier float64 `json:"risk_multiplier"`

	// MinSpotRatio is the hard lower bound for Spot exposure (0..1).
	// The controller will not reduce Spot ratio below this value.
	MinSpotRatio float64 `json:"min_spot_ratio"`

	// MaxSpotRatio is the hard upper bound for Spot exposure (0..1).
	// The controller will not increase Spot ratio above this value.
	MaxSpotRatio float64 `json:"max_spot_ratio"`

	// TargetSpotRatio is the preferred operating point (soft target, 0..1).
	// When market is safe, the controller will slowly drift toward this value.
	TargetSpotRatio float64 `json:"target_spot_ratio"`

	// StepMinutes defines the duration represented by one control step.
	// Used for migration-cooldown and time-since-migration normalization.
	StepMinutes int `json:"step_minutes"`

	// PolicyMode controls action selection path.
	// Supported values: "rl", "deterministic".
	PolicyMode string `json:"policy_mode"`

	// RLShadowEnabled controls whether RL shadow comparison telemetry is recorded
	// while deterministic mode is active. If omitted, deterministic mode defaults
	// to enabled (to preserve current rollout behavior), and RL mode defaults off.
	RLShadowEnabled *bool `json:"rl_shadow_enabled,omitempty"`

	// DeterministicPolicy configures the TFT-risk + workload rule engine.
	// The runtime-side pool safety vector contract consumed by this policy is
	// documented by PoolSafetyVector above. Phase 1 does not add extra JSON
	// knobs for those live fields; the vector is populated from cluster state.
	DeterministicPolicy DeterministicPolicyConfig `json:"deterministic_policy"`
}

// LoadRuntimeConfig loads the runtime configuration from the specified path.
func LoadRuntimeConfig(path string) (*RuntimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime config: %w", err)
	}

	var cfg RuntimeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse runtime config: %w", err)
	}

	applyRuntimeDefaults(&cfg)
	applyRuntimeClamps(&cfg)
	if err := validateRuntimeConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// DefaultRuntimeConfig returns a safe default runtime config.
func DefaultRuntimeConfig() *RuntimeConfig {
	cfg := RuntimeConfig{}
	applyRuntimeDefaults(&cfg)
	applyRuntimeClamps(&cfg)
	return &cfg
}

func applyRuntimeDefaults(cfg *RuntimeConfig) {
	// Core defaults
	if cfg.RiskMultiplier == 0 {
		cfg.RiskMultiplier = 1.0
	}
	if cfg.MaxSpotRatio == 0 {
		cfg.MaxSpotRatio = 1.0
	}
	if cfg.StepMinutes <= 0 {
		cfg.StepMinutes = 10
	}

	mode := strings.TrimSpace(strings.ToLower(cfg.PolicyMode))
	if mode == "" {
		mode = PolicyModeDeterministic
	}
	cfg.PolicyMode = mode

	// Deterministic policy defaults
	dp := &cfg.DeterministicPolicy
	if dp.EmergencyRiskThreshold == 0 {
		dp.EmergencyRiskThreshold = 0.90
	}
	if dp.RuntimeEmergencyThreshold == 0 {
		dp.RuntimeEmergencyThreshold = 0.80
	}
	if dp.HighRiskThreshold == 0 {
		dp.HighRiskThreshold = 0.60
	}
	if dp.MediumRiskThreshold == 0 {
		dp.MediumRiskThreshold = 0.35
	}
	if dp.MinSavingsRatioForIncrease == 0 {
		dp.MinSavingsRatioForIncrease = 0.15
	}
	if dp.MaxPaybackHoursForIncrease == 0 {
		dp.MaxPaybackHoursForIncrease = 6.0
	}
	if strings.TrimSpace(dp.OODMode) == "" {
		dp.OODMode = "conservative"
	}
	if dp.OODMaxRiskForIncrease == 0 {
		dp.OODMaxRiskForIncrease = 0.25
	}
	if dp.OODMinSavingsRatioForIncrease == 0 {
		dp.OODMinSavingsRatioForIncrease = 0.25
	}
	if dp.OODMaxPaybackHoursForIncrease == 0 {
		dp.OODMaxPaybackHoursForIncrease = 3.0
	}
	if dp.TargetSpotRatioDriftAlpha == nil {
		dp.TargetSpotRatioDriftAlpha = float64Ptr(defaultTargetSpotRatioDriftAlpha)
	}
	if len(dp.PriorityCapRules) == 0 {
		dp.PriorityCapRules = defaultPriorityCapRules()
	}
	if len(dp.OutagePenaltyCapRules) == 0 {
		dp.OutagePenaltyCapRules = defaultOutagePenaltyCapRules()
	}
	if len(dp.StartupTimeCapRules) == 0 {
		dp.StartupTimeCapRules = defaultStartupTimeCapRules()
	}
	if len(dp.MigrationCostCapRules) == 0 {
		dp.MigrationCostCapRules = defaultMigrationCostCapRules()
	}
	if len(dp.UtilizationCapRules) == 0 {
		dp.UtilizationCapRules = defaultUtilizationCapRules()
	}

	// Buckets from workload distributions (preferred) when not explicitly set.
	if dp.FeatureBuckets.Source == "" {
		dp.FeatureBuckets.Source = "config/workload_distributions.yaml"
	}
	if dp.FeatureBuckets.empty() {
		if fromFile, err := loadFeatureBucketsFromWorkloadDistribution(dp.FeatureBuckets.Source); err == nil && !fromFile.empty() {
			dp.FeatureBuckets = fromFile
			dp.FeatureBuckets.Source = "config/workload_distributions.yaml"
		}
	}
	if dp.FeatureBuckets.empty() {
		dp.FeatureBuckets = defaultFeatureBuckets()
	}
}

func applyRuntimeClamps(cfg *RuntimeConfig) {
	// Safety clamping for RiskMultiplier
	if cfg.RiskMultiplier < 0.1 {
		cfg.RiskMultiplier = 0.1
	}
	if cfg.RiskMultiplier > 10.0 {
		cfg.RiskMultiplier = 10.0
	}

	// Set sensible defaults for ratio fields if not specified (0 is considered unset for max)
	if cfg.MaxSpotRatio == 0 {
		cfg.MaxSpotRatio = 1.0
	}

	// Clamp ratio fields to valid range [0, 1]
	cfg.MinSpotRatio = clampFloat(cfg.MinSpotRatio, 0, 1)
	cfg.MaxSpotRatio = clampFloat(cfg.MaxSpotRatio, 0, 1)
	cfg.TargetSpotRatio = clampFloat(cfg.TargetSpotRatio, 0, 1)

	// Ensure min <= max
	if cfg.MinSpotRatio > cfg.MaxSpotRatio {
		cfg.MinSpotRatio = cfg.MaxSpotRatio
	}

	// Clamp target to be within min/max bounds
	if cfg.TargetSpotRatio < cfg.MinSpotRatio {
		cfg.TargetSpotRatio = cfg.MinSpotRatio
	}
	if cfg.TargetSpotRatio > cfg.MaxSpotRatio {
		cfg.TargetSpotRatio = cfg.MaxSpotRatio
	}

	// Keep control cadence in sane bounds.
	if cfg.StepMinutes < 1 {
		cfg.StepMinutes = 1
	}
	if cfg.StepMinutes > 120 {
		cfg.StepMinutes = 120
	}

	// Normalize policy mode.
	switch strings.ToLower(strings.TrimSpace(cfg.PolicyMode)) {
	case PolicyModeDeterministic:
		cfg.PolicyMode = PolicyModeDeterministic
	case PolicyModeRL:
		cfg.PolicyMode = PolicyModeRL
	default:
		cfg.PolicyMode = PolicyModeDeterministic
	}

	// Clamp deterministic thresholds.
	dp := &cfg.DeterministicPolicy
	dp.EmergencyRiskThreshold = clampFloat(dp.EmergencyRiskThreshold, 0, 1)
	dp.RuntimeEmergencyThreshold = clampFloat(dp.RuntimeEmergencyThreshold, 0, 1)
	dp.HighRiskThreshold = clampFloat(dp.HighRiskThreshold, 0, 1)
	dp.MediumRiskThreshold = clampFloat(dp.MediumRiskThreshold, 0, 1)

	// Ensure monotonic risk bands: emergency >= high >= medium.
	if dp.HighRiskThreshold > dp.EmergencyRiskThreshold {
		dp.HighRiskThreshold = dp.EmergencyRiskThreshold
	}
	if dp.MediumRiskThreshold > dp.HighRiskThreshold {
		dp.MediumRiskThreshold = dp.HighRiskThreshold
	}

	dp.MinSavingsRatioForIncrease = clampFloat(dp.MinSavingsRatioForIncrease, 0, 1)
	dp.MaxPaybackHoursForIncrease = clampFloat(dp.MaxPaybackHoursForIncrease, 0.1, 168)

	dp.OODMode = strings.ToLower(strings.TrimSpace(dp.OODMode))
	if dp.OODMode == "" {
		dp.OODMode = "conservative"
	}
	dp.OODMaxRiskForIncrease = clampFloat(dp.OODMaxRiskForIncrease, 0, 1)
	dp.OODMinSavingsRatioForIncrease = clampFloat(dp.OODMinSavingsRatioForIncrease, 0, 1)
	dp.OODMaxPaybackHoursForIncrease = clampFloat(dp.OODMaxPaybackHoursForIncrease, 0.1, 168)
	dp.TargetSpotRatioDriftAlpha = float64Ptr(dp.ResolvedTargetSpotRatioDriftAlpha())
	dp.PriorityCapRules = dp.ResolvedPriorityCapRules()
	dp.OutagePenaltyCapRules = dp.ResolvedOutagePenaltyCapRules()
	dp.StartupTimeCapRules = dp.ResolvedStartupTimeCapRules()
	dp.MigrationCostCapRules = dp.ResolvedMigrationCostCapRules()
	dp.UtilizationCapRules = dp.ResolvedUtilizationCapRules()

	dp.FeatureBuckets.PodStartupTimeSeconds = normalizeBoundaries(dp.FeatureBuckets.PodStartupTimeSeconds)
	dp.FeatureBuckets.OutagePenaltyHours = normalizeBoundaries(dp.FeatureBuckets.OutagePenaltyHours)
	dp.FeatureBuckets.PriorityScore = normalizeBoundaries(dp.FeatureBuckets.PriorityScore)
	dp.FeatureBuckets.ClusterUtilization = normalizeBoundaries(dp.FeatureBuckets.ClusterUtilization)

	if len(dp.FeatureBuckets.PodStartupTimeSeconds) < 2 ||
		len(dp.FeatureBuckets.OutagePenaltyHours) < 2 ||
		len(dp.FeatureBuckets.PriorityScore) < 2 ||
		len(dp.FeatureBuckets.ClusterUtilization) < 2 {
		dp.FeatureBuckets = defaultFeatureBuckets()
	}
}

// clampFloat clamps a value to the given range [min, max].
func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// ClampSpotRatio clamps a target spot ratio to the configured min/max bounds.
func (c *RuntimeConfig) ClampSpotRatio(ratio float64) float64 {
	return clampFloat(ratio, c.MinSpotRatio, c.MaxSpotRatio)
}

// UseDeterministicPolicy reports whether deterministic policy mode is enabled.
func (c *RuntimeConfig) UseDeterministicPolicy() bool {
	if c == nil {
		return false
	}
	return strings.EqualFold(c.PolicyMode, PolicyModeDeterministic)
}

// UseRLShadow reports whether RL shadow comparison should be recorded.
// This only returns true when deterministic mode is active.
func (c *RuntimeConfig) UseRLShadow() bool {
	if c == nil || !c.UseDeterministicPolicy() {
		return false
	}
	// Preserve existing deterministic-active + RL-shadow behavior when unset.
	if c.RLShadowEnabled == nil {
		return true
	}
	return *c.RLShadowEnabled
}

func validateRuntimeConfig(cfg *RuntimeConfig) error {
	if cfg == nil {
		return nil
	}
	// Current implementation supports RL shadow comparison only when deterministic
	// is the active policy. Reject unsupported combinations explicitly.
	if strings.EqualFold(cfg.PolicyMode, PolicyModeRL) && cfg.RLShadowEnabled != nil && *cfg.RLShadowEnabled {
		return fmt.Errorf("invalid runtime config: rl_shadow_enabled=true requires policy_mode=%q", PolicyModeDeterministic)
	}
	return nil
}

// DeterministicPolicyConfig controls deterministic action selection.
type DeterministicPolicyConfig struct {
	EmergencyRiskThreshold        float64            `json:"emergency_risk_threshold"`
	RuntimeEmergencyThreshold     float64            `json:"runtime_emergency_threshold"`
	HighRiskThreshold             float64            `json:"high_risk_threshold"`
	MediumRiskThreshold           float64            `json:"medium_risk_threshold"`
	MinSavingsRatioForIncrease    float64            `json:"min_savings_ratio_for_increase"`
	MaxPaybackHoursForIncrease    float64            `json:"max_payback_hours_for_increase"`
	OODMode                       string             `json:"ood_mode"`
	OODMaxRiskForIncrease         float64            `json:"ood_max_risk_for_increase"`
	OODMinSavingsRatioForIncrease float64            `json:"ood_min_savings_ratio_for_increase"`
	OODMaxPaybackHoursForIncrease float64            `json:"ood_max_payback_hours_for_increase"`
	TargetSpotRatioDriftAlpha     *float64           `json:"target_spot_ratio_drift_alpha,omitempty"`
	PriorityCapRules              []SpotRatioCapRule `json:"priority_cap_rules"`
	OutagePenaltyCapRules         []SpotRatioCapRule `json:"outage_penalty_cap_rules"`
	StartupTimeCapRules           []SpotRatioCapRule `json:"startup_time_cap_rules"`
	MigrationCostCapRules         []SpotRatioCapRule `json:"migration_cost_cap_rules"`
	UtilizationCapRules           []SpotRatioCapRule `json:"utilization_cap_rules"`
	FeatureBuckets                FeatureBuckets     `json:"feature_buckets"`
}

// SpotRatioCapRule maps a feature threshold to a maximum allowed spot ratio.
// Rules are evaluated in descending threshold order; first match wins.
type SpotRatioCapRule struct {
	Threshold    float64 `json:"threshold"`
	MaxSpotRatio float64 `json:"max_spot_ratio"`
}

// ResolvedTargetSpotRatioDriftAlpha returns the configured drift alpha or the
// runtime default when the field is omitted.
func (p DeterministicPolicyConfig) ResolvedTargetSpotRatioDriftAlpha() float64 {
	if p.TargetSpotRatioDriftAlpha == nil {
		return defaultTargetSpotRatioDriftAlpha
	}
	return clampFloat(*p.TargetSpotRatioDriftAlpha, 0, 1)
}

// ResolvedPriorityCapRules returns normalized priority cap rules or defaults.
func (p DeterministicPolicyConfig) ResolvedPriorityCapRules() []SpotRatioCapRule {
	return resolvedCapRules(p.PriorityCapRules, defaultPriorityCapRules(), true)
}

// ResolvedOutagePenaltyCapRules returns normalized outage penalty cap rules or defaults.
func (p DeterministicPolicyConfig) ResolvedOutagePenaltyCapRules() []SpotRatioCapRule {
	return resolvedCapRules(p.OutagePenaltyCapRules, defaultOutagePenaltyCapRules(), false)
}

// ResolvedStartupTimeCapRules returns normalized startup time cap rules or defaults.
func (p DeterministicPolicyConfig) ResolvedStartupTimeCapRules() []SpotRatioCapRule {
	return resolvedCapRules(p.StartupTimeCapRules, defaultStartupTimeCapRules(), false)
}

// ResolvedMigrationCostCapRules returns normalized migration cost cap rules or defaults.
func (p DeterministicPolicyConfig) ResolvedMigrationCostCapRules() []SpotRatioCapRule {
	return resolvedCapRules(p.MigrationCostCapRules, defaultMigrationCostCapRules(), false)
}

// ResolvedUtilizationCapRules returns normalized utilization cap rules or defaults.
func (p DeterministicPolicyConfig) ResolvedUtilizationCapRules() []SpotRatioCapRule {
	return resolvedCapRules(p.UtilizationCapRules, defaultUtilizationCapRules(), true)
}

// FeatureBuckets defines known value ranges for OOD detection.
type FeatureBuckets struct {
	Source                string    `json:"source"`
	PodStartupTimeSeconds []float64 `json:"pod_startup_time_seconds"`
	OutagePenaltyHours    []float64 `json:"outage_penalty_hours"`
	PriorityScore         []float64 `json:"priority_score"`
	ClusterUtilization    []float64 `json:"cluster_utilization"`
}

func (b FeatureBuckets) empty() bool {
	return len(b.PodStartupTimeSeconds) == 0 &&
		len(b.OutagePenaltyHours) == 0 &&
		len(b.PriorityScore) == 0 &&
		len(b.ClusterUtilization) == 0
}

func defaultFeatureBuckets() FeatureBuckets {
	return FeatureBuckets{
		Source:                "default",
		PodStartupTimeSeconds: []float64{0, 60, 120, 300, 600, 1200},
		OutagePenaltyHours:    []float64{0, 1, 4, 10, 24, 48, 96},
		PriorityScore:         []float64{0.0, 0.25, 0.5, 0.75, 1.0},
		ClusterUtilization:    []float64{0.0, 0.5, 0.7, 0.85, 0.95, 1.0},
	}
}

func defaultPriorityCapRules() []SpotRatioCapRule {
	return []SpotRatioCapRule{
		{Threshold: 0.90, MaxSpotRatio: 0.20},
		{Threshold: 0.70, MaxSpotRatio: 0.50},
		{Threshold: 0.45, MaxSpotRatio: 0.80},
	}
}

func defaultOutagePenaltyCapRules() []SpotRatioCapRule {
	return []SpotRatioCapRule{
		{Threshold: 96.0, MaxSpotRatio: 0.10},
		{Threshold: 48.0, MaxSpotRatio: 0.20},
		{Threshold: 24.0, MaxSpotRatio: 0.30},
		{Threshold: 10.0, MaxSpotRatio: 0.50},
	}
}

func defaultStartupTimeCapRules() []SpotRatioCapRule {
	return []SpotRatioCapRule{
		{Threshold: 600.0, MaxSpotRatio: 0.20},
		{Threshold: 300.0, MaxSpotRatio: 0.30},
		{Threshold: 120.0, MaxSpotRatio: 0.50},
	}
}

func defaultMigrationCostCapRules() []SpotRatioCapRule {
	return []SpotRatioCapRule{
		{Threshold: 8.0, MaxSpotRatio: 0.20},
		{Threshold: 5.0, MaxSpotRatio: 0.30},
		{Threshold: 2.0, MaxSpotRatio: 0.60},
	}
}

func defaultUtilizationCapRules() []SpotRatioCapRule {
	return []SpotRatioCapRule{
		{Threshold: 0.95, MaxSpotRatio: 0.70},
	}
}

func resolvedCapRules(configured, fallback []SpotRatioCapRule, clampThresholdToUnit bool) []SpotRatioCapRule {
	if len(configured) == 0 {
		return cloneCapRules(fallback)
	}
	return normalizeCapRules(configured, clampThresholdToUnit)
}

func normalizeCapRules(rules []SpotRatioCapRule, clampThresholdToUnit bool) []SpotRatioCapRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]SpotRatioCapRule, 0, len(rules))
	for _, rule := range rules {
		threshold := rule.Threshold
		if threshold < 0 {
			threshold = 0
		}
		if clampThresholdToUnit {
			threshold = clampFloat(threshold, 0, 1)
		}
		out = append(out, SpotRatioCapRule{
			Threshold:    threshold,
			MaxSpotRatio: clampFloat(rule.MaxSpotRatio, 0, 1),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Threshold == out[j].Threshold {
			return out[i].MaxSpotRatio < out[j].MaxSpotRatio
		}
		return out[i].Threshold > out[j].Threshold
	})
	return out
}

func cloneCapRules(rules []SpotRatioCapRule) []SpotRatioCapRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]SpotRatioCapRule, len(rules))
	copy(out, rules)
	return out
}

func float64Ptr(v float64) *float64 {
	return &v
}

func normalizeBoundaries(values []float64) []float64 {
	if len(values) == 0 {
		return nil
	}
	out := make([]float64, 0, len(values))
	for _, v := range values {
		if v < 0 {
			v = 0
		}
		out = append(out, v)
	}
	sort.Float64s(out)
	uniq := out[:0]
	var prev float64
	for i, v := range out {
		if i == 0 || v != prev {
			uniq = append(uniq, v)
			prev = v
		}
	}
	return uniq
}

type workloadDistributionConfig struct {
	WorkloadProfileBounds struct {
		Overall struct {
			PodStartupTime struct {
				Min float64 `yaml:"min"`
				P05 float64 `yaml:"p05"`
				P50 float64 `yaml:"p50"`
				P95 float64 `yaml:"p95"`
				Max float64 `yaml:"max"`
			} `yaml:"pod_startup_time"`
			OutagePenaltyHours struct {
				Min float64 `yaml:"min"`
				P05 float64 `yaml:"p05"`
				P50 float64 `yaml:"p50"`
				P95 float64 `yaml:"p95"`
				Max float64 `yaml:"max"`
			} `yaml:"outage_penalty_hours"`
			ClusterUtilizationTypical struct {
				Min float64 `yaml:"min"`
				P05 float64 `yaml:"p05"`
				P50 float64 `yaml:"p50"`
				P95 float64 `yaml:"p95"`
				Max float64 `yaml:"max"`
			} `yaml:"cluster_utilization_typical"`
		} `yaml:"overall"`
	} `yaml:"workload_profile_bounds"`
}

func loadFeatureBucketsFromWorkloadDistribution(path string) (FeatureBuckets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FeatureBuckets{}, err
	}
	var cfg workloadDistributionConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return FeatureBuckets{}, err
	}

	b := FeatureBuckets{
		Source: path,
		PodStartupTimeSeconds: normalizeBoundaries([]float64{
			cfg.WorkloadProfileBounds.Overall.PodStartupTime.Min,
			cfg.WorkloadProfileBounds.Overall.PodStartupTime.P05,
			cfg.WorkloadProfileBounds.Overall.PodStartupTime.P50,
			cfg.WorkloadProfileBounds.Overall.PodStartupTime.P95,
			cfg.WorkloadProfileBounds.Overall.PodStartupTime.Max,
		}),
		OutagePenaltyHours: normalizeBoundaries([]float64{
			cfg.WorkloadProfileBounds.Overall.OutagePenaltyHours.Min,
			cfg.WorkloadProfileBounds.Overall.OutagePenaltyHours.P05,
			cfg.WorkloadProfileBounds.Overall.OutagePenaltyHours.P50,
			cfg.WorkloadProfileBounds.Overall.OutagePenaltyHours.P95,
			cfg.WorkloadProfileBounds.Overall.OutagePenaltyHours.Max,
		}),
		PriorityScore: normalizeBoundaries([]float64{0.0, 0.25, 0.5, 0.75, 1.0}),
		ClusterUtilization: normalizeBoundaries([]float64{
			cfg.WorkloadProfileBounds.Overall.ClusterUtilizationTypical.Min,
			cfg.WorkloadProfileBounds.Overall.ClusterUtilizationTypical.P05,
			cfg.WorkloadProfileBounds.Overall.ClusterUtilizationTypical.P50,
			cfg.WorkloadProfileBounds.Overall.ClusterUtilizationTypical.P95,
			cfg.WorkloadProfileBounds.Overall.ClusterUtilizationTypical.Max,
		}),
	}
	if b.empty() {
		return FeatureBuckets{}, fmt.Errorf("workload_profile_bounds.overall missing or empty in %s", path)
	}
	return b, nil
}
