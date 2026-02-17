package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
	k8sfake "k8s.io/client-go/kubernetes/fake"
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
		Cloud: cloud,
		PriceProvider: &MockPriceProvider{
			PriceData: cloudapi.SpotPriceData{
				CurrentPrice:  0.2,
				OnDemandPrice: 1.0,
				PriceHistory:  []float64{0.2, 0.2, 0.2},
			},
		},
		PrometheusClient:    &metrics.Client{},
		Inference:           &inference.InferenceEngine{},
		Logger:              slog.Default(),
		RiskThreshold:       0.6,
		MaxDrainRatio:       0.2,
		ReconcileInterval:   10 * time.Second,
		ConfidenceThreshold: 0.5,
	}
}

func TestNew_RequiresPriceProvider(t *testing.T) {
	cfg := baseControllerConfig(&noopCloudProvider{dryRun: true})
	cfg.PriceProvider = nil

	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected nil PriceProvider to be rejected")
	}
	if !strings.Contains(err.Error(), "price provider is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_BlocksSyntheticPricesAlways(t *testing.T) {
	t.Setenv("SPOTVORTEX_PRICE_MODE", "synthetic")

	_, err := New(baseControllerConfig(&noopCloudProvider{dryRun: true}))
	if err == nil {
		t.Fatal("expected synthetic price mode to be blocked even in dry-run")
	}
	if !strings.Contains(err.Error(), "synthetic price mode is not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_BlocksSyntheticMetricsWhenLive(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	_, err := New(baseControllerConfig(&noopCloudProvider{dryRun: false}))
	if err == nil {
		t.Fatal("expected synthetic metrics to be blocked in live mode")
	}
	if !strings.Contains(err.Error(), "synthetic metrics mode is blocked") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_AllowsSyntheticMetricsInDryRun(t *testing.T) {
	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")

	controller, err := New(baseControllerConfig(&noopCloudProvider{dryRun: true}))
	if err != nil {
		t.Fatalf("expected dry-run controller to allow synthetic metrics: %v", err)
	}
	if !controller.useSyntheticMetrics {
		t.Fatal("expected synthetic metrics mode to be enabled in dry-run")
	}
}

func TestNew_ConfiguresDrainerDryRunFromCloud(t *testing.T) {
	tests := []struct {
		name      string
		cloudMode bool
	}{
		{
			name:      "shadow mode",
			cloudMode: true,
		},
		{
			name:      "active mode",
			cloudMode: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseControllerConfig(&noopCloudProvider{dryRun: tc.cloudMode})
			cfg.K8sClient = k8sfake.NewSimpleClientset()

			ctrl, err := New(cfg)
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			if ctrl.drain == nil {
				t.Fatal("expected drainer to be initialized when k8s client is configured")
			}
			if ctrl.drain.config.DryRun != tc.cloudMode {
				t.Fatalf("drainer dry-run=%v, want %v", ctrl.drain.config.DryRun, tc.cloudMode)
			}
		})
	}
}
