package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadRuntimeConfig_WithRatioFields(t *testing.T) {
	// Create temp config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "runtime.json")

	// Write test config
	content := `{
		"risk_multiplier": 0.8,
		"min_spot_ratio": 0.1,
		"max_spot_ratio": 0.7,
		"target_spot_ratio": 0.4,
		"step_minutes": 30,
		"policy_mode": "deterministic",
		"deterministic_policy": {
			"high_risk_threshold": 0.65
		}
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}

	// Verify values
	if cfg.RiskMultiplier != 0.8 {
		t.Errorf("RiskMultiplier: got %v, want 0.8", cfg.RiskMultiplier)
	}
	if cfg.MinSpotRatio != 0.1 {
		t.Errorf("MinSpotRatio: got %v, want 0.1", cfg.MinSpotRatio)
	}
	if cfg.MaxSpotRatio != 0.7 {
		t.Errorf("MaxSpotRatio: got %v, want 0.7", cfg.MaxSpotRatio)
	}
	if cfg.TargetSpotRatio != 0.4 {
		t.Errorf("TargetSpotRatio: got %v, want 0.4", cfg.TargetSpotRatio)
	}
	if cfg.StepMinutes != 30 {
		t.Errorf("StepMinutes: got %v, want 30", cfg.StepMinutes)
	}
	if !cfg.UseDeterministicPolicy() {
		t.Fatalf("expected deterministic policy mode to be enabled")
	}
	if cfg.DeterministicPolicy.HighRiskThreshold != 0.65 {
		t.Errorf("HighRiskThreshold: got %v, want 0.65", cfg.DeterministicPolicy.HighRiskThreshold)
	}
}

func TestLoadRuntimeConfig_DefaultMaxSpotRatio(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "runtime.json")

	// Config with max_spot_ratio = 0 (should default to 1.0)
	content := `{"risk_multiplier": 1.0}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}

	if cfg.MaxSpotRatio != 1.0 {
		t.Errorf("MaxSpotRatio should default to 1.0, got %v", cfg.MaxSpotRatio)
	}
	if cfg.StepMinutes != 10 {
		t.Errorf("StepMinutes should default to 10, got %v", cfg.StepMinutes)
	}
	if cfg.PolicyMode != PolicyModeDeterministic {
		t.Errorf("PolicyMode should default to %q, got %q", PolicyModeDeterministic, cfg.PolicyMode)
	}
}

func TestLoadRuntimeConfig_ClampMinGreaterThanMax(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "runtime.json")

	// Invalid: min > max
	content := `{
		"risk_multiplier": 1.0,
		"min_spot_ratio": 0.8,
		"max_spot_ratio": 0.3
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}

	// min should be clamped to max
	if cfg.MinSpotRatio != cfg.MaxSpotRatio {
		t.Errorf("MinSpotRatio should be clamped to MaxSpotRatio, got min=%v, max=%v",
			cfg.MinSpotRatio, cfg.MaxSpotRatio)
	}
}

func TestClampSpotRatio(t *testing.T) {
	cfg := &RuntimeConfig{
		MinSpotRatio: 0.2,
		MaxSpotRatio: 0.8,
	}

	tests := []struct {
		input    float64
		expected float64
	}{
		{0.0, 0.2}, // below min
		{0.2, 0.2}, // at min
		{0.5, 0.5}, // in range
		{0.8, 0.8}, // at max
		{1.0, 0.8}, // above max
	}

	for _, tc := range tests {
		got := cfg.ClampSpotRatio(tc.input)
		if got != tc.expected {
			t.Errorf("ClampSpotRatio(%v): got %v, want %v", tc.input, got, tc.expected)
		}
	}
}

func TestLoadRuntimeConfig_InvalidPolicyModeFallsBackToDeterministic(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "runtime.json")

	content := `{
		"risk_multiplier": 1.0,
		"policy_mode": "something_else"
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}
	if cfg.PolicyMode != PolicyModeDeterministic {
		t.Fatalf("expected policy mode %q, got %q", PolicyModeDeterministic, cfg.PolicyMode)
	}
}

func TestLoadRuntimeConfig_LoadsBucketsFromWorkloadDistribution(t *testing.T) {
	tmpDir := t.TempDir()
	distPath := filepath.Join(tmpDir, "workload_distributions.yaml")
	configPath := filepath.Join(tmpDir, "runtime.json")

	dist := `
workload_profile_bounds:
  overall:
    pod_startup_time:
      min: 5
      p05: 10
      p50: 30
      p95: 90
      max: 200
    outage_penalty_hours:
      min: 0.4
      p05: 0.5
      p50: 1.5
      p95: 8
      max: 24
    cluster_utilization_typical:
      min: 0.3
      p05: 0.5
      p50: 0.7
      p95: 0.9
      max: 1.0
`
	if err := os.WriteFile(distPath, []byte(dist), 0644); err != nil {
		t.Fatalf("failed to write dist config: %v", err)
	}

	content := `{
		"policy_mode": "deterministic",
		"deterministic_policy": {
			"feature_buckets": {
				"source": "` + distPath + `"
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write runtime config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}

	b := cfg.DeterministicPolicy.FeatureBuckets
	if len(b.PodStartupTimeSeconds) < 5 {
		t.Fatalf("expected pod startup buckets from workload distribution, got %+v", b.PodStartupTimeSeconds)
	}
	if b.PodStartupTimeSeconds[0] != 5 || b.PodStartupTimeSeconds[len(b.PodStartupTimeSeconds)-1] != 200 {
		t.Fatalf("unexpected pod startup bucket bounds: %+v", b.PodStartupTimeSeconds)
	}
	if b.OutagePenaltyHours[0] != 0.4 || b.OutagePenaltyHours[len(b.OutagePenaltyHours)-1] != 24 {
		t.Fatalf("unexpected outage bucket bounds: %+v", b.OutagePenaltyHours)
	}
	if b.ClusterUtilization[0] != 0.3 || b.ClusterUtilization[len(b.ClusterUtilization)-1] != 1.0 {
		t.Fatalf("unexpected utilization bucket bounds: %+v", b.ClusterUtilization)
	}
}

func TestLoadRuntimeConfig_DeterministicCapRulesAndDriftAlpha(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "runtime.json")

	content := `{
		"policy_mode": "deterministic",
		"deterministic_policy": {
			"target_spot_ratio_drift_alpha": 0.0,
			"priority_cap_rules": [
				{"threshold": 0.40, "max_spot_ratio": 0.55},
				{"threshold": 0.90, "max_spot_ratio": 0.20}
			],
			"startup_time_cap_rules": [
				{"threshold": -5, "max_spot_ratio": 0.60},
				{"threshold": 300, "max_spot_ratio": 1.20}
			],
			"utilization_cap_rules": [
				{"threshold": 1.40, "max_spot_ratio": 0.70},
				{"threshold": 0.80, "max_spot_ratio": -0.10}
			]
		}
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write runtime config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig failed: %v", err)
	}

	if got := cfg.DeterministicPolicy.ResolvedTargetSpotRatioDriftAlpha(); got != 0.0 {
		t.Fatalf("expected explicit zero drift alpha to be preserved, got %.2f", got)
	}

	wantPriority := []SpotRatioCapRule{
		{Threshold: 0.90, MaxSpotRatio: 0.20},
		{Threshold: 0.40, MaxSpotRatio: 0.55},
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.PriorityCapRules, wantPriority) {
		t.Fatalf("unexpected priority cap rules: got %+v want %+v", cfg.DeterministicPolicy.PriorityCapRules, wantPriority)
	}

	wantStartup := []SpotRatioCapRule{
		{Threshold: 300, MaxSpotRatio: 1.0},
		{Threshold: 0, MaxSpotRatio: 0.60},
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.StartupTimeCapRules, wantStartup) {
		t.Fatalf("unexpected startup cap rules: got %+v want %+v", cfg.DeterministicPolicy.StartupTimeCapRules, wantStartup)
	}

	wantUtilization := []SpotRatioCapRule{
		{Threshold: 1.0, MaxSpotRatio: 0.70},
		{Threshold: 0.80, MaxSpotRatio: 0.0},
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.UtilizationCapRules, wantUtilization) {
		t.Fatalf("unexpected utilization cap rules: got %+v want %+v", cfg.DeterministicPolicy.UtilizationCapRules, wantUtilization)
	}

	if !reflect.DeepEqual(cfg.DeterministicPolicy.OutagePenaltyCapRules, defaultOutagePenaltyCapRules()) {
		t.Fatalf("expected outage penalty cap defaults, got %+v", cfg.DeterministicPolicy.OutagePenaltyCapRules)
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.MigrationCostCapRules, defaultMigrationCostCapRules()) {
		t.Fatalf("expected migration cost cap defaults, got %+v", cfg.DeterministicPolicy.MigrationCostCapRules)
	}
}

func TestDefaultRuntimeConfig_AndPolicyHelpers(t *testing.T) {
	cfg := DefaultRuntimeConfig()
	if cfg == nil {
		t.Fatal("expected default config, got nil")
	}
	if cfg.RiskMultiplier <= 0 {
		t.Fatalf("expected positive default risk multiplier, got %v", cfg.RiskMultiplier)
	}
	if cfg.StepMinutes <= 0 {
		t.Fatalf("expected positive default step minutes, got %d", cfg.StepMinutes)
	}
	if cfg.PolicyMode != PolicyModeDeterministic {
		t.Fatalf("expected default policy mode %q, got %q", PolicyModeDeterministic, cfg.PolicyMode)
	}
	if !cfg.UseDeterministicPolicy() {
		t.Fatal("default config should enable deterministic policy")
	}
	if !cfg.UseRLShadow() {
		t.Fatal("default deterministic mode should enable RL shadow when unset")
	}

	var nilCfg *RuntimeConfig
	if nilCfg.UseDeterministicPolicy() {
		t.Fatal("nil config should not enable deterministic policy")
	}

	disabled := false
	cfg.RLShadowEnabled = &disabled
	if cfg.UseRLShadow() {
		t.Fatal("expected explicit rl_shadow_enabled=false to disable RL shadow")
	}
}

func TestDefaultRuntimeConfig_DeterministicPolicyContractDefaults(t *testing.T) {
	cfg := DefaultRuntimeConfig()

	if got := cfg.DeterministicPolicy.ResolvedTargetSpotRatioDriftAlpha(); got != defaultTargetSpotRatioDriftAlpha {
		t.Fatalf("unexpected default drift alpha: got %.2f want %.2f", got, defaultTargetSpotRatioDriftAlpha)
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.PriorityCapRules, defaultPriorityCapRules()) {
		t.Fatalf("unexpected default priority cap rules: %+v", cfg.DeterministicPolicy.PriorityCapRules)
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.OutagePenaltyCapRules, defaultOutagePenaltyCapRules()) {
		t.Fatalf("unexpected default outage cap rules: %+v", cfg.DeterministicPolicy.OutagePenaltyCapRules)
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.StartupTimeCapRules, defaultStartupTimeCapRules()) {
		t.Fatalf("unexpected default startup cap rules: %+v", cfg.DeterministicPolicy.StartupTimeCapRules)
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.MigrationCostCapRules, defaultMigrationCostCapRules()) {
		t.Fatalf("unexpected default migration cap rules: %+v", cfg.DeterministicPolicy.MigrationCostCapRules)
	}
	if !reflect.DeepEqual(cfg.DeterministicPolicy.UtilizationCapRules, defaultUtilizationCapRules()) {
		t.Fatalf("unexpected default utilization cap rules: %+v", cfg.DeterministicPolicy.UtilizationCapRules)
	}
}

func TestShippedRuntimeConfig_MatchesDeterministicReleaseContract(t *testing.T) {
	configPath := filepath.Clean(filepath.Join("..", "..", "config", "runtime.json"))

	cfg, err := LoadRuntimeConfig(configPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig(%s) failed: %v", configPath, err)
	}

	if cfg.PolicyMode != PolicyModeDeterministic {
		t.Fatalf("PolicyMode: got %q, want %q", cfg.PolicyMode, PolicyModeDeterministic)
	}
	if cfg.StepMinutes != 10 {
		t.Fatalf("StepMinutes: got %d, want 10", cfg.StepMinutes)
	}
	if cfg.MinSpotRatio != 0.167 {
		t.Fatalf("MinSpotRatio: got %.3f, want 0.167", cfg.MinSpotRatio)
	}
	if cfg.MaxSpotRatio != 1.0 {
		t.Fatalf("MaxSpotRatio: got %.3f, want 1.0", cfg.MaxSpotRatio)
	}
	if !cfg.UseRLShadow() {
		t.Fatal("expected deterministic shipped config to keep RL shadow enabled")
	}
}

func TestLoadRuntimeConfig_RLShadowToggleAndInvalidCombination(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("deterministic explicit false", func(t *testing.T) {
		configPath := filepath.Join(tmpDir, "runtime-shadow-off.json")
		content := `{
			"policy_mode": "deterministic",
			"rl_shadow_enabled": false
		}`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write test config: %v", err)
		}
		cfg, err := LoadRuntimeConfig(configPath)
		if err != nil {
			t.Fatalf("LoadRuntimeConfig failed: %v", err)
		}
		if cfg.UseRLShadow() {
			t.Fatal("expected rl shadow disabled when rl_shadow_enabled=false")
		}
	})

	t.Run("invalid rl active plus shadow enabled", func(t *testing.T) {
		configPath := filepath.Join(tmpDir, "runtime-invalid-shadow.json")
		content := `{
			"policy_mode": "rl",
			"rl_shadow_enabled": true
		}`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write test config: %v", err)
		}
		if _, err := LoadRuntimeConfig(configPath); err == nil {
			t.Fatal("expected invalid config error for policy_mode=rl with rl_shadow_enabled=true")
		}
	})
}

func TestLoadRuntimeConfig_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.json")

	if err := os.WriteFile(configPath, []byte("{invalid-json"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadRuntimeConfig(configPath)
	if err == nil {
		t.Error("expected error loading invalid json, got nil")
	}
}

func TestLoadRuntimeConfig_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "missing.json")

	_, err := LoadRuntimeConfig(configPath)
	if err == nil {
		t.Error("expected error loading missing file, got nil")
	}
}

func TestNormalizeBoundaries(t *testing.T) {
	tests := []struct {
		input []float64
		want  []float64
	}{
		{[]float64{10, 5, 20}, []float64{5, 10, 20}},
		{[]float64{5, 5, 10}, []float64{5, 10}},
		{[]float64{-5, 10}, []float64{0, 10}},
		{nil, nil},
		{[]float64{}, nil},
	}

	for _, tc := range tests {
		got := normalizeBoundaries(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("normalizeBoundaries(%v): got %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("normalizeBoundaries(%v): got %v, want %v", tc.input, got, tc.want)
				break
			}
		}
	}
}

func TestFeatureBuckets_Empty(t *testing.T) {
	b := FeatureBuckets{}
	if !b.empty() {
		t.Error("expected empty buckets to return true")
	}
	b.PodStartupTimeSeconds = []float64{1}
	if b.empty() {
		t.Error("expected non-empty buckets to return false")
	}
}

func TestPoolSafetyVector_DefaultAndNormalize(t *testing.T) {
	def := DefaultPoolSafetyVector()
	if def.SafeMaxSpotRatio != 1.0 {
		t.Fatalf("expected default safe max spot ratio 1.0, got %.2f", def.SafeMaxSpotRatio)
	}
	if def.ZoneDiversificationScore != 1.0 {
		t.Fatalf("expected default zone diversification 1.0, got %.2f", def.ZoneDiversificationScore)
	}
	if def.EvictablePodFraction != 1.0 {
		t.Fatalf("expected default evictable fraction 1.0, got %.2f", def.EvictablePodFraction)
	}

	raw := PoolSafetyVector{
		CriticalServiceSpotConcentration: 1.5,
		StatefulPodFraction:              -0.2,
		RestartP95Seconds:                -5,
		RecoveryBudgetViolationRisk:      2.0,
		SpareODHeadroomNodes:             -1,
		ZoneDiversificationScore:         2.0,
		EvictablePodFraction:             -1,
		SafeMaxSpotRatio:                 1.2,
	}

	got := NormalizePoolSafetyVector(raw)
	if got.CriticalServiceSpotConcentration != 1.0 {
		t.Fatalf("expected concentration clamp to 1.0, got %.2f", got.CriticalServiceSpotConcentration)
	}
	if got.StatefulPodFraction != 0.0 {
		t.Fatalf("expected stateful fraction clamp to 0.0, got %.2f", got.StatefulPodFraction)
	}
	if got.RestartP95Seconds != 0.0 {
		t.Fatalf("expected restart clamp to 0.0, got %.2f", got.RestartP95Seconds)
	}
	if got.RecoveryBudgetViolationRisk != 1.0 {
		t.Fatalf("expected recovery risk clamp to 1.0, got %.2f", got.RecoveryBudgetViolationRisk)
	}
	if got.SpareODHeadroomNodes != 0.0 {
		t.Fatalf("expected headroom clamp to 0.0, got %.2f", got.SpareODHeadroomNodes)
	}
	if got.ZoneDiversificationScore != 1.0 {
		t.Fatalf("expected zone score clamp to 1.0, got %.2f", got.ZoneDiversificationScore)
	}
	if got.EvictablePodFraction != 0.0 {
		t.Fatalf("expected evictable fraction clamp to 0.0, got %.2f", got.EvictablePodFraction)
	}
	if got.SafeMaxSpotRatio != 1.0 {
		t.Fatalf("expected safe max clamp to 1.0, got %.2f", got.SafeMaxSpotRatio)
	}
	if !(PoolSafetyVector{}).IsZero() {
		t.Fatal("expected zero vector to report IsZero=true")
	}
}
