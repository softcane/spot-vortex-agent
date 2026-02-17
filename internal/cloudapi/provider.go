// Package cloudapi provides abstractions for cloud provider operations.
// All operations support dry-run mode per the Prime Directive.
package cloudapi

import (
	"context"
	"time"
)

// DrainRequest represents a request to drain a node.
type DrainRequest struct {
	NodeID      string
	Zone        string
	GracePeriod time.Duration
	Force       bool
}

// DrainResult represents the outcome of a drain operation.
type DrainResult struct {
	NodeID      string
	Success     bool
	DryRun      bool
	Duration    time.Duration
	PodsEvicted int
	Error       error
}

// ProvisionRequest represents a request to provision a new instance.
type ProvisionRequest struct {
	InstanceType       string
	Zone               string
	SpotPrice          float64
	FallbackToOnDemand bool
}

// ProvisionResult represents the outcome of a provision operation.
type ProvisionResult struct {
	InstanceID   string
	InstanceType string
	Zone         string
	IsSpot       bool
	DryRun       bool
	Error        error
}

// CloudProvider defines the interface for cloud operations.
// All implementations MUST respect the DryRun flag.
type CloudProvider interface {
	// Drain cordons and drains the specified node.
	// In dry-run mode, logs the action without executing.
	Drain(ctx context.Context, req DrainRequest) (*DrainResult, error)

	// Provision requests a new instance (Spot or On-Demand fallback).
	// In dry-run mode, logs the action without executing.
	Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error)

	// IsDryRun returns whether the provider is in dry-run mode.
	IsDryRun() bool
}
