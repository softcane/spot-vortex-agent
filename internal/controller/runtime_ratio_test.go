package controller

import (
	"testing"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
)

func TestApplyTargetSpotRatioWithRuntimeKillSwitch(t *testing.T) {
	c := &Controller{
		targetSpotRatio: map[string]float64{
			"pool-a": 0.70,
		},
	}

	killSwitch := &config.RuntimeConfig{
		MinSpotRatio:    0.0,
		MaxSpotRatio:    0.0,
		TargetSpotRatio: 0.0,
	}

	c.applyTargetSpotRatioWithConfig("pool-a", inference.ActionIncrease30, killSwitch, false)

	got := c.targetSpotRatio["pool-a"]
	if got != 0.0 {
		t.Fatalf("expected kill-switch clamp to 0.0, got %.4f", got)
	}
}

func TestApplyTargetSpotRatioWithRuntimeTargetLerp(t *testing.T) {
	c := &Controller{
		targetSpotRatio: map[string]float64{
			"pool-a": 0.80,
		},
	}

	runtimeCfg := &config.RuntimeConfig{
		MinSpotRatio:    0.0,
		MaxSpotRatio:    1.0,
		TargetSpotRatio: 0.20,
	}

	c.applyTargetSpotRatioWithConfig("pool-a", inference.ActionHold, runtimeCfg, true)

	got := c.targetSpotRatio["pool-a"]
	want := 0.74 // 0.8 lerp toward 0.2 by alpha=0.1
	if diff := got - want; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("unexpected lerp result: got %.6f want %.6f", got, want)
	}
}

func TestStepsSinceMigration_UsesConfiguredStepMinutes(t *testing.T) {
	last := time.Now().Add(-95 * time.Minute)
	got := stepsSinceMigration(last, 30)
	if got != 3 {
		t.Fatalf("expected 3 steps for 95 minutes at 30-minute steps, got %d", got)
	}
}

func TestStepsSinceMigration_DefaultsAndClamps(t *testing.T) {
	last := time.Now().Add(-25 * time.Minute)
	got := stepsSinceMigration(last, 0)
	if got != 2 {
		t.Fatalf("expected default 10-minute step conversion to 2, got %d", got)
	}

	future := time.Now().Add(40 * time.Minute)
	got = stepsSinceMigration(future, 10)
	if got != 0 {
		t.Fatalf("expected future timestamp to clamp at 0, got %d", got)
	}
}
