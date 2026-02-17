package cmd

import "testing"

func TestValidateSyntheticModePolicy_BlocksSyntheticPricesAlways(t *testing.T) {
	// Synthetic prices must be blocked even in dry-run mode.
	err := validateSyntheticModePolicy(true, false, true, "", "synthetic")
	if err == nil {
		t.Fatal("expected synthetic price mode to be blocked even in dry-run")
	}
}

func TestValidateSyntheticModePolicy_BlocksSyntheticPricesInLiveMode(t *testing.T) {
	err := validateSyntheticModePolicy(false, false, true, "", "synthetic")
	if err == nil {
		t.Fatal("expected synthetic price mode to be blocked in live mode")
	}
}

func TestValidateSyntheticModePolicy_BlocksSyntheticMetricsInLiveMode(t *testing.T) {
	err := validateSyntheticModePolicy(false, true, false, "synthetic", "")
	if err == nil {
		t.Fatal("expected synthetic metrics to be blocked in live mode")
	}
}

func TestValidateSyntheticModePolicy_AllowsSyntheticMetricsInDryRun(t *testing.T) {
	err := validateSyntheticModePolicy(true, true, false, "synthetic", "")
	if err != nil {
		t.Fatalf("expected synthetic metrics to be allowed in dry-run: %v", err)
	}
}

func TestValidateSyntheticModePolicy_AllowsLiveRealModes(t *testing.T) {
	err := validateSyntheticModePolicy(false, false, false, "", "")
	if err != nil {
		t.Fatalf("expected live mode without synthetic settings to pass: %v", err)
	}
}

func TestValidateSyntheticModePolicy_AllowsDryRunRealModes(t *testing.T) {
	err := validateSyntheticModePolicy(true, false, false, "", "")
	if err != nil {
		t.Fatalf("expected dry-run with real modes to pass: %v", err)
	}
}
