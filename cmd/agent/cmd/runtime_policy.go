package cmd

import "fmt"

func validateSyntheticModePolicy(isDryRun, useSyntheticMetrics, useSyntheticPrices bool, metricsMode, priceMode string) error {
	if !isDryRun && (useSyntheticMetrics || useSyntheticPrices) {
		return fmt.Errorf("synthetic telemetry modes are blocked when --dry-run is false: SPOTVORTEX_METRICS_MODE=%q SPOTVORTEX_PRICE_MODE=%q", metricsMode, priceMode)
	}
	return nil
}
