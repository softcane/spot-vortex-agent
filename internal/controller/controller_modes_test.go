package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pradeepsingh/spot-vortex-agent/internal/cloudapi"
	"github.com/pradeepsingh/spot-vortex-agent/internal/inference"
	"github.com/pradeepsingh/spot-vortex-agent/internal/metrics"
)

type noopCloudProvider struct {
	dryRun bool
}

func (n *noopCloudProvider) Drain(ctx context.Context, req cloudapi.DrainRequest) (*cloudapi.DrainResult, error) {
	return &cloudapi.DrainResult{NodeID: req.NodeID, Success: true, DryRun: n.dryRun}, nil
}

func (n *noopCloudProvider) Provision(ctx context.Context, req cloudapi.ProvisionRequest) (*cloudapi.ProvisionResult, error) {
	return &cloudapi.ProvisionResult{InstanceType: req.InstanceType, Zone: req.Zone, IsSpot: true, DryRun: n.dryRun}, nil
}

func (n *noopCloudProvider) IsDryRun() bool {
	return n.dryRun
}

func baseControllerConfig(cloud cloudapi.CloudProvider) Config {
	return Config{
		Cloud:               cloud,
		PrometheusClient:    &metrics.Client{},
		Inference:           &inference.InferenceEngine{},
		Logger:              slog.Default(),
		RiskThreshold:       0.6,
		MaxDrainRatio:       0.2,
		ReconcileInterval:   10 * time.Second,
		ConfidenceThreshold: 0.5,
	}
}

func TestNew_BlocksSyntheticModesWhenLive(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")
	t.Setenv("SPOTVORTEX_PRICE_MODE", "synthetic")

	_, err := New(baseControllerConfig(&noopCloudProvider{dryRun: false}))
	if err == nil {
		t.Fatal("expected synthetic-mode policy error in live mode")
	}
	if !strings.Contains(err.Error(), "synthetic telemetry modes are blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_AllowsSyntheticModesInDryRun(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")
	t.Setenv("SPOTVORTEX_PRICE_MODE", "synthetic")

	controller, err := New(baseControllerConfig(&noopCloudProvider{dryRun: true}))
	if err != nil {
		t.Fatalf("expected dry-run controller to allow synthetic modes: %v", err)
	}
	if !controller.useSyntheticMetrics {
		t.Fatal("expected synthetic metrics mode to be enabled in dry-run")
	}
	if !controller.useSyntheticPrices {
		t.Fatal("expected synthetic price mode to be enabled in dry-run")
	}
}
