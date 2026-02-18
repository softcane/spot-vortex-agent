package cmd

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/softcane/spot-vortex-agent/internal/config"
)

func TestResolveRuntimePriceProvider_UsesFakeFromInlineJSON(t *testing.T) {
	t.Setenv(e2eSuiteEnvVar, "karpenter-local")
	t.Setenv(fakePriceProviderJSONEnv, `{
  "default": {"current_price": 0.20, "on_demand_price": 1.00}
}`)
	t.Setenv(fakePriceProviderFileEnv, "")

	resolved, err := resolveRuntimePriceProvider(context.Background(), &config.Config{}, slog.Default(), true)
	if err != nil {
		t.Fatalf("resolveRuntimePriceProvider failed: %v", err)
	}
	if !resolved.isFake {
		t.Fatal("expected fake price provider to be active")
	}

	price, err := resolved.provider.GetSpotPrice(context.Background(), "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice failed: %v", err)
	}
	if price.CurrentPrice != 0.20 || price.OnDemandPrice != 1.00 {
		t.Fatalf("unexpected fake price data: %+v", price)
	}
}

func TestResolveRuntimePriceProvider_UsesFakeFromFile(t *testing.T) {
	t.Setenv(e2eSuiteEnvVar, "karpenter-local")
	t.Setenv(fakePriceProviderJSONEnv, "")

	file, err := os.CreateTemp(t.TempDir(), "fake-prices-*.json")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	content := `{"default":{"current_price":0.30,"on_demand_price":1.30}}`
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("write temp scenario failed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp scenario failed: %v", err)
	}
	t.Setenv(fakePriceProviderFileEnv, file.Name())

	resolved, err := resolveRuntimePriceProvider(context.Background(), &config.Config{}, slog.Default(), true)
	if err != nil {
		t.Fatalf("resolveRuntimePriceProvider failed: %v", err)
	}
	if !resolved.isFake {
		t.Fatal("expected fake price provider to be active")
	}
}

func TestResolveRuntimePriceProvider_RejectsFakeInLiveMode(t *testing.T) {
	t.Setenv(e2eSuiteEnvVar, "karpenter-local")
	t.Setenv(fakePriceProviderJSONEnv, `{"default":{"current_price":0.20,"on_demand_price":1.00}}`)
	t.Setenv(fakePriceProviderFileEnv, "")

	_, err := resolveRuntimePriceProvider(context.Background(), &config.Config{}, slog.Default(), false)
	if err == nil || !strings.Contains(err.Error(), "--dry-run=true") {
		t.Fatalf("expected dry-run guard error, got: %v", err)
	}
}

func TestResolveRuntimePriceProvider_RejectsFakeWithoutSuiteGuard(t *testing.T) {
	t.Setenv(e2eSuiteEnvVar, "")
	t.Setenv(fakePriceProviderJSONEnv, `{"default":{"current_price":0.20,"on_demand_price":1.00}}`)
	t.Setenv(fakePriceProviderFileEnv, "")

	_, err := resolveRuntimePriceProvider(context.Background(), &config.Config{}, slog.Default(), true)
	if err == nil || !strings.Contains(err.Error(), e2eSuiteEnvVar) {
		t.Fatalf("expected suite guard error, got: %v", err)
	}
}

func TestResolveRuntimePriceProvider_RejectsDualFakeSources(t *testing.T) {
	t.Setenv(fakePriceProviderJSONEnv, `{"default":{"current_price":0.20,"on_demand_price":1.00}}`)
	t.Setenv(fakePriceProviderFileEnv, "/tmp/fake-prices.json")

	_, err := resolveRuntimePriceProvider(context.Background(), &config.Config{}, slog.Default(), true)
	if err == nil || !strings.Contains(err.Error(), "set only one") {
		t.Fatalf("expected dual-source validation error, got: %v", err)
	}
}
