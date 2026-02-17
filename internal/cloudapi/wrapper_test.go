package cloudapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/pradeepsingh/spot-vortex-agent/internal/cloudapi"
)

func TestSpotWrapper_Drain_DryRun(t *testing.T) {
	wrapper := cloudapi.NewSpotWrapper(cloudapi.SpotWrapperConfig{
		DryRun: true,
	})

	req := cloudapi.DrainRequest{
		NodeID:      "node-abc123",
		Zone:        "us-east-1a",
		GracePeriod: 30 * time.Second,
		Force:       false,
	}

	result, err := wrapper.Drain(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.DryRun {
		t.Error("expected DryRun=true in result")
	}

	if !result.Success {
		t.Error("expected Success=true in dry-run mode")
	}

	if result.NodeID != req.NodeID {
		t.Errorf("expected NodeID=%q, got %q", req.NodeID, result.NodeID)
	}
}

func TestSpotWrapper_Provision_DryRun(t *testing.T) {
	wrapper := cloudapi.NewSpotWrapper(cloudapi.SpotWrapperConfig{
		DryRun: true,
	})

	req := cloudapi.ProvisionRequest{
		InstanceType:       "m5.large",
		Zone:               "us-west-2b",
		SpotPrice:          0.05,
		FallbackToOnDemand: true,
	}

	result, err := wrapper.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.DryRun {
		t.Error("expected DryRun=true in result")
	}

	if result.InstanceType != req.InstanceType {
		t.Errorf("expected InstanceType=%q, got %q", req.InstanceType, result.InstanceType)
	}

	if result.Zone != req.Zone {
		t.Errorf("expected Zone=%q, got %q", req.Zone, result.Zone)
	}
}

func TestSpotWrapper_LiveMode_NoProvider(t *testing.T) {
	wrapper := cloudapi.NewSpotWrapper(cloudapi.SpotWrapperConfig{
		DryRun: false, // Live mode
		// No provider configured
	})

	_, err := wrapper.Drain(context.Background(), cloudapi.DrainRequest{
		NodeID: "node-test",
		Zone:   "us-east-1a",
	})

	if err != cloudapi.ErrNoProvider {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
}

func TestSpotWrapper_IsDryRun(t *testing.T) {
	tests := []struct {
		name     string
		dryRun   bool
		expected bool
	}{
		{"dry-run enabled", true, true},
		{"dry-run disabled", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapper := cloudapi.NewSpotWrapper(cloudapi.SpotWrapperConfig{
				DryRun: tt.dryRun,
			})

			if got := wrapper.IsDryRun(); got != tt.expected {
				t.Errorf("IsDryRun() = %v, want %v", got, tt.expected)
			}
		})
	}
}
