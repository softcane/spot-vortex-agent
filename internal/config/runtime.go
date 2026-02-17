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
)

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

	// DeterministicPolicy configures the TFT-risk + workload rule engine.
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
		mode = PolicyModeRL
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
	default:
		cfg.PolicyMode = PolicyModeRL
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

// DeterministicPolicyConfig controls deterministic action selection.
type DeterministicPolicyConfig struct {
	EmergencyRiskThreshold        float64        `json:"emergency_risk_threshold"`
	RuntimeEmergencyThreshold     float64        `json:"runtime_emergency_threshold"`
	HighRiskThreshold             float64        `json:"high_risk_threshold"`
	MediumRiskThreshold           float64        `json:"medium_risk_threshold"`
	MinSavingsRatioForIncrease    float64        `json:"min_savings_ratio_for_increase"`
	MaxPaybackHoursForIncrease    float64        `json:"max_payback_hours_for_increase"`
	OODMode                       string         `json:"ood_mode"`
	OODMaxRiskForIncrease         float64        `json:"ood_max_risk_for_increase"`
	OODMinSavingsRatioForIncrease float64        `json:"ood_min_savings_ratio_for_increase"`
	OODMaxPaybackHoursForIncrease float64        `json:"ood_max_payback_hours_for_increase"`
	FeatureBuckets                FeatureBuckets `json:"feature_buckets"`
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
