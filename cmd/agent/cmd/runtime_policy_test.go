package cmd

import "testing"

func TestValidateSyntheticModePolicy_BlocksLiveSyntheticModes(t *testing.T) {
	err := validateSyntheticModePolicy(false, true, false, "synthetic", "")
	if err == nil {
		t.Fatal("expected live-mode synthetic policy to fail")
	}
}

func TestValidateSyntheticModePolicy_AllowsDryRunSyntheticModes(t *testing.T) {
	err := validateSyntheticModePolicy(true, true, true, "synthetic", "synthetic")
	if err != nil {
		t.Fatalf("expected dry-run synthetic mode to be allowed: %v", err)
	}
}

func TestValidateSyntheticModePolicy_AllowsLiveRealModes(t *testing.T) {
	err := validateSyntheticModePolicy(false, false, false, "", "")
	if err != nil {
		t.Fatalf("expected live mode without synthetic settings to pass: %v", err)
	}
}
