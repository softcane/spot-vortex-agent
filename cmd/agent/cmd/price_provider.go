package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/config"
)

const (
	fakePriceProviderFileEnv = "SPOTVORTEX_TEST_PRICE_PROVIDER_FILE"
	fakePriceProviderJSONEnv = "SPOTVORTEX_TEST_PRICE_PROVIDER_JSON"
	e2eSuiteEnvVar           = "SPOTVORTEX_E2E_SUITE"
)

type runtimePriceProvider struct {
	provider cloudapi.PriceProvider
	isFake   bool
}

func resolveRuntimePriceProvider(ctx context.Context, cfg *config.Config, logger *slog.Logger, dryRun bool) (runtimePriceProvider, error) {
	fakeFile := strings.TrimSpace(os.Getenv(fakePriceProviderFileEnv))
	fakeJSON := strings.TrimSpace(os.Getenv(fakePriceProviderJSONEnv))

	if fakeFile != "" && fakeJSON != "" {
		return runtimePriceProvider{}, fmt.Errorf("set only one of %s or %s", fakePriceProviderFileEnv, fakePriceProviderJSONEnv)
	}

	if fakeFile != "" || fakeJSON != "" {
		if !dryRun {
			return runtimePriceProvider{}, fmt.Errorf("fake price provider is test-only and requires --dry-run=true")
		}
		if strings.TrimSpace(os.Getenv(e2eSuiteEnvVar)) == "" {
			return runtimePriceProvider{}, fmt.Errorf("fake price provider requires %s to be set (test suite guard)", e2eSuiteEnvVar)
		}

		var (
			provider cloudapi.PriceProvider
			err      error
		)
		if fakeFile != "" {
			provider, err = cloudapi.NewFakePriceProviderFromFile(fakeFile)
			if err != nil {
				return runtimePriceProvider{}, fmt.Errorf("load fake price provider from file: %w", err)
			}
		} else {
			provider, err = cloudapi.NewFakePriceProviderFromJSON(fakeJSON)
			if err != nil {
				return runtimePriceProvider{}, fmt.Errorf("load fake price provider from inline json: %w", err)
			}
		}
		logger.Info("using test-only fake price provider",
			"source_file", fakeFile,
			"suite", os.Getenv(e2eSuiteEnvVar),
		)
		return runtimePriceProvider{provider: provider, isFake: true}, nil
	}

	priceProvider, _, err := cloudapi.NewAutoDetectedPriceProvider(ctx, logger)
	if err != nil {
		logger.Warn("failed to auto-detect cloud provider, attempting AWS fallback", "error", err)
		priceProvider, err = cloudapi.NewAWSPriceProvider(ctx, awsRegionFromConfig(cfg), logger)
		if err != nil {
			return runtimePriceProvider{}, fmt.Errorf("initialize real price provider: %w", err)
		}
	}

	return runtimePriceProvider{provider: priceProvider, isFake: false}, nil
}

func awsRegionFromConfig(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.AWS.Region) != "" {
		return cfg.AWS.Region
	}
	return "us-east-1"
}
