// Package metrics provides a Prometheus client for querying node metrics.
// Privacy: Only metadata (CPU/Mem) is collected per mission_guardrail.md.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// NodeMetrics represents CPU and memory metrics for a single node.
type NodeMetrics struct {
	NodeID             string    `json:"node_id"`
	Zone               string    `json:"zone"`
	InstanceType       string    `json:"instance_type"`
	CPUUsagePercent    float64   `json:"cpu_usage_percent"`
	MemoryUsagePercent float64   `json:"memory_usage_percent"`
	SpotPrice          float64   `json:"spot_price"`
	OnDemandPrice      float64   `json:"on_demand_price"`
	IsSpot             bool      `json:"is_spot"`
	Timestamp          time.Time `json:"timestamp"`
}

// Client wraps the Prometheus API for node metrics queries.
type Client struct {
	api    v1.API
	logger *slog.Logger
}

// ClientConfig holds configuration for the metrics client.
type ClientConfig struct {
	PrometheusURL string
	Logger        *slog.Logger
	// API is an optional Prometheus API client. If nil, one will be created from PrometheusURL.
	// Useful for testing.
	API v1.API
}

// NewClient creates a new Prometheus metrics client.
// PRODUCTION ONLY - no dry-run mode.
func NewClient(cfg ClientConfig) (*Client, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var v1api v1.API
	if cfg.API != nil {
		v1api = cfg.API
	} else {
		if cfg.PrometheusURL == "" {
			return nil, fmt.Errorf("PrometheusURL is required")
		}

		client, err := api.NewClient(api.Config{
			Address: cfg.PrometheusURL,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create prometheus client: %w", err)
		}
		v1api = v1.NewAPI(client)
	}

	return &Client{
		api:    v1api,
		logger: logger,
	}, nil
}

// GetNodeMetrics queries Prometheus for current node CPU and memory usage.
// PRODUCTION ONLY - returns real metrics.
func (c *Client) GetNodeMetrics(ctx context.Context) ([]NodeMetrics, error) {
	c.logger.Debug("fetching node metrics from prometheus")

	// Query CPU usage (1 - idle rate)
	cpuMetrics, err := c.queryCPUUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query CPU metrics: %w", err)
	}

	// Query memory usage
	memMetrics, err := c.queryMemoryUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query memory metrics: %w", err)
	}

	// Merge CPU and memory metrics by node
	return c.mergeMetrics(cpuMetrics, memMetrics), nil
}

// queryCPUUsage queries node CPU utilization percentage.
func (c *Client) queryCPUUsage(ctx context.Context) (map[string]float64, error) {
	// PromQL: 100 - (avg by (node) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)
	query := `100 - (avg by (node) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`

	result, warnings, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, err
	}

	if len(warnings) > 0 {
		c.logger.Warn("prometheus query warnings", "warnings", warnings)
	}

	return c.extractNodeValues(result), nil
}

// queryMemoryUsage queries node memory utilization percentage.
func (c *Client) queryMemoryUsage(ctx context.Context) (map[string]float64, error) {
	// PromQL: (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) * 100
	query := `(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes) * 100`

	result, warnings, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, err
	}

	if len(warnings) > 0 {
		c.logger.Warn("prometheus query warnings", "warnings", warnings)
	}

	return c.extractNodeValues(result), nil
}

// extractNodeValues extracts node-keyed values from Prometheus query result.
func (c *Client) extractNodeValues(result model.Value) map[string]float64 {
	values := make(map[string]float64)

	vector, ok := result.(model.Vector)
	if !ok {
		c.logger.Warn("unexpected prometheus result type", "type", result.Type())
		return values
	}

	for _, sample := range vector {
		nodeLabel := string(sample.Metric["node"])
		if nodeLabel == "" {
			nodeLabel = string(sample.Metric["instance"])
		}
		if nodeLabel != "" {
			values[nodeLabel] = float64(sample.Value)
		}
	}

	return values
}

// mergeMetrics combines CPU and memory metrics into NodeMetrics slice.
func (c *Client) mergeMetrics(cpu, mem map[string]float64) []NodeMetrics {
	now := time.Now()
	var result []NodeMetrics

	// Get all unique nodes
	nodes := make(map[string]struct{})
	for n := range cpu {
		nodes[n] = struct{}{}
	}
	for n := range mem {
		nodes[n] = struct{}{}
	}

	for node := range nodes {
		result = append(result, NodeMetrics{
			NodeID:             node,
			Zone:               "", // Would be populated from node labels
			InstanceType:       "", // Would be populated from node labels
			CPUUsagePercent:    cpu[node],
			MemoryUsagePercent: mem[node],
			Timestamp:          now,
		})
	}

	return result
}

// GetClusterUtilization returns the average cluster-wide CPU utilization (0.0 to 1.0).
// This is used by the RL model to make migration decisions:
// - Low utilization (<40%): More aggressive spot migration, plenty of headroom
// - High utilization (>80%): Conservative, avoid drains that could cause pod pending
func (c *Client) GetClusterUtilization(ctx context.Context) (float64, error) {
	// PromQL: avg(100 - (avg by (node) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100))
	query := `avg(100 - (avg by (node) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100))`

	result, warnings, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("failed to query cluster utilization: %w", err)
	}

	if len(warnings) > 0 {
		c.logger.Warn("prometheus query warnings", "warnings", warnings)
	}

	// Extract scalar value
	switch v := result.(type) {
	case model.Vector:
		if len(v) > 0 {
			return float64(v[0].Value) / 100.0, nil // Convert to 0-1 range
		}
	case *model.Scalar:
		return float64(v.Value) / 100.0, nil
	}

	c.logger.Debug("no cluster utilization data available")
	return 0.5, nil // Default to 50% if no data
}

// GetPoolUtilization returns average CPU utilization per pool (by instance_type:zone).
// Returns a map of poolID -> utilization (0.0 to 1.0).
func (c *Client) GetPoolUtilization(ctx context.Context) (map[string]float64, error) {
	// Query CPU usage grouped by instance type and zone labels
	// Note: This assumes node_exporter exports these labels or they're added via relabeling
	query := `avg by (node_kubernetes_io_instance_type, topology_kubernetes_io_zone) (
		100 - (avg by (node, node_kubernetes_io_instance_type, topology_kubernetes_io_zone)
			(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)
	)`

	result, warnings, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		// Fall back to cluster-wide utilization if pool-level query fails
		c.logger.Debug("pool-level utilization query failed, falling back to cluster-wide", "error", err)
		clusterUtil, clusterErr := c.GetClusterUtilization(ctx)
		if clusterErr != nil {
			return nil, clusterErr
		}
		// Return single entry that will be used as default
		return map[string]float64{"default": clusterUtil}, nil
	}

	if len(warnings) > 0 {
		c.logger.Warn("prometheus query warnings", "warnings", warnings)
	}

	poolUtils := make(map[string]float64)

	vector, ok := result.(model.Vector)
	if !ok {
		c.logger.Debug("unexpected prometheus result type for pool utilization", "type", result.Type())
		return poolUtils, nil
	}

	for _, sample := range vector {
		instanceType := string(sample.Metric["node_kubernetes_io_instance_type"])
		zone := string(sample.Metric["topology_kubernetes_io_zone"])

		if instanceType == "" {
			instanceType = "unknown"
		}
		if zone == "" {
			zone = "unknown"
		}

		poolID := fmt.Sprintf("%s:%s", instanceType, zone)
		poolUtils[poolID] = float64(sample.Value) / 100.0 // Convert to 0-1 range
	}

	return poolUtils, nil
}
