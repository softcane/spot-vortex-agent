package config

import (
	"os"
	"path/filepath"
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
	if cfg.PolicyMode != PolicyModeRL {
		t.Errorf("PolicyMode should default to %q, got %q", PolicyModeRL, cfg.PolicyMode)
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

func TestLoadRuntimeConfig_InvalidPolicyModeFallsBackToRL(t *testing.T) {
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
	if cfg.PolicyMode != PolicyModeRL {
		t.Fatalf("expected policy mode %q, got %q", PolicyModeRL, cfg.PolicyMode)
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
	if cfg.PolicyMode != PolicyModeRL {
		t.Fatalf("expected default policy mode %q, got %q", PolicyModeRL, cfg.PolicyMode)
	}
	if cfg.UseDeterministicPolicy() {
		t.Fatal("default config should not enable deterministic policy")
	}

	var nilCfg *RuntimeConfig
	if nilCfg.UseDeterministicPolicy() {
		t.Fatal("nil config should not enable deterministic policy")
	}

	cfg.PolicyMode = PolicyModeDeterministic
	if !cfg.UseDeterministicPolicy() {
		t.Fatal("expected deterministic policy helper to return true")
	}
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
