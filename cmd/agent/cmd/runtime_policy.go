package cmd

import "fmt"

// validateSyntheticModePolicy enforces runtime policy for synthetic telemetry modes.
//
// Policy:
//   - SPOTVORTEX_PRICE_MODE=synthetic -> always blocked (shadow mode requires real market data)
//   - SPOTVORTEX_METRICS_MODE=synthetic -> allowed only when --dry-run=true (for local Kind testing)
func validateSyntheticModePolicy(isDryRun, useSyntheticMetrics, useSyntheticPrices bool, metricsMode, priceMode string) error {
	// Synthetic prices are unconditionally blocked: shadow mode requires real AWS
	// ec2:DescribeSpotPriceHistory / pricing:GetProducts data.
	if useSyntheticPrices {
		return fmt.Errorf("synthetic price mode is not supported: SPOTVORTEX_PRICE_MODE=%q; "+
			"shadow mode requires real market data (see docs/IAM_PERMISSIONS.md)", priceMode)
	}

	// Synthetic metrics are allowed only in dry-run mode (for local Kind testing).
	if useSyntheticMetrics && !isDryRun {
		return fmt.Errorf("synthetic metrics mode is blocked when --dry-run is false: "+
			"SPOTVORTEX_METRICS_MODE=%q", metricsMode)
	}

	return nil
}
