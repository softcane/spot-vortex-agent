package cloudapi

import "errors"

// Sentinel errors for cloud operations.
var (
	// ErrNoProvider is returned when attempting live operations without a configured provider.
	ErrNoProvider = errors.New("cloudapi: no provider configured for live operations")

	// ErrDrainFailed is returned when a node drain operation fails.
	ErrDrainFailed = errors.New("cloudapi: node drain failed")

	// ErrProvisionFailed is returned when instance provisioning fails.
	ErrProvisionFailed = errors.New("cloudapi: instance provision failed")

	// ErrSpotUnavailable is returned when Spot capacity is unavailable.
	ErrSpotUnavailable = errors.New("cloudapi: spot instance unavailable, consider On-Demand fallback")
)
