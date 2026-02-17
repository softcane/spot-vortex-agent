// Package controller provides savings calculation for SpotVortex dry-run mode.
// Enables customers to see potential value before enabling active management.
package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
)

// NodeSavings represents potential savings for a single node.
type NodeSavings struct {
	NodeID       string
	PoolID       string
	InstanceType string
	Zone         string
	IsSpot       bool

	// Pricing
	CurrentCostHourly float64 // Current hourly cost (spot or OD)
	SpotPriceHourly   float64
	ODPriceHourly     float64

	// Savings calculation
	SavingsHourly  float64 // If migrated to spot
	SavingsDaily   float64
	SavingsMonthly float64

	// Assessment
	Action        inference.Action
	CapacityScore float32
	Confidence    float32
	RiskLevel     string // "low", "medium", "high"

	// Recommendation
	Recommendation string
	CanMigrate     bool
}

// PoolSavings represents aggregated savings for a workload pool.
type PoolSavings struct {
	PoolID         string
	Zone           string
	TotalNodes     int
	SpotNodes      int
	ODNodes        int
	OptimizableOD  int // OD nodes that could migrate to spot

	// Aggregated savings
	CurrentCostHourly    float64
	OptimalCostHourly    float64
	PotentialSavingsHour float64
	PotentialSavingsDay  float64
	PotentialSavingsMonth float64

	// Risk assessment
	PoolRiskScore float32
	PoolAction    inference.Action

	// Node breakdown
	NodeSavings []NodeSavings
}

// SavingsReport contains the complete dry-run savings analysis.
type SavingsReport struct {
	Pools            map[string]*PoolSavings
	TotalSavingsHour float64
	TotalSavingsDay  float64
	TotalSavingsMonth float64
	TotalNodes       int
	OptimizableNodes int
}

// calculateNodeSavings computes potential savings for a single node.
func calculateNodeSavings(
	nodeID, poolID, instanceType, zone string,
	isSpot bool,
	spotPrice, odPrice float64,
	action inference.Action,
	capacityScore, confidence float32,
) NodeSavings {
	ns := NodeSavings{
		NodeID:        nodeID,
		PoolID:        poolID,
		InstanceType:  instanceType,
		Zone:          zone,
		IsSpot:        isSpot,
		SpotPriceHourly: spotPrice,
		ODPriceHourly:   odPrice,
		Action:        action,
		CapacityScore: capacityScore,
		Confidence:    confidence,
	}

	// Current cost depends on whether node is spot or OD
	if isSpot {
		ns.CurrentCostHourly = spotPrice
	} else {
		ns.CurrentCostHourly = odPrice
	}

	// Calculate potential savings (only for OD nodes that could migrate)
	if !isSpot && spotPrice > 0 && odPrice > spotPrice {
		ns.SavingsHourly = odPrice - spotPrice
		ns.SavingsDaily = ns.SavingsHourly * 24
		ns.SavingsMonthly = ns.SavingsDaily * 30
		ns.CanMigrate = true
	}

	// Risk level based on capacity score
	switch {
	case capacityScore < 0.3:
		ns.RiskLevel = "low"
	case capacityScore < 0.7:
		ns.RiskLevel = "medium"
	default:
		ns.RiskLevel = "high"
	}

	// Generate recommendation
	ns.Recommendation = generateNodeRecommendation(ns)

	return ns
}

// generateNodeRecommendation creates a human-readable recommendation.
func generateNodeRecommendation(ns NodeSavings) string {
	if ns.IsSpot {
		switch ns.RiskLevel {
		case "low":
			return fmt.Sprintf("Node is on spot ($%.3f/hr). Market is stable - optimal placement.", ns.CurrentCostHourly)
		case "medium":
			return fmt.Sprintf("Node is on spot ($%.3f/hr). Monitor - market shows some volatility.", ns.CurrentCostHourly)
		default:
			return fmt.Sprintf("Node is on spot ($%.3f/hr). HIGH RISK - consider migration to on-demand.", ns.CurrentCostHourly)
		}
	}

	// On-demand node
	if ns.CanMigrate && ns.RiskLevel == "low" {
		return fmt.Sprintf(
			"Node is on-demand ($%.3f/hr). Could save $%.3f/hr ($%.2f/month) by migrating to spot. Market is stable.",
			ns.CurrentCostHourly, ns.SavingsHourly, ns.SavingsMonthly,
		)
	} else if ns.CanMigrate && ns.RiskLevel == "medium" {
		return fmt.Sprintf(
			"Node is on-demand ($%.3f/hr). Potential savings: $%.3f/hr. Market has some volatility - migration optional.",
			ns.CurrentCostHourly, ns.SavingsHourly,
		)
	}
	return fmt.Sprintf("Node is on-demand ($%.3f/hr). Currently optimal due to market conditions.", ns.CurrentCostHourly)
}

// aggregatePoolSavings aggregates node savings into pool-level summary.
func aggregatePoolSavings(poolID, zone string, nodes []NodeSavings, poolAction inference.Action, poolRisk float32) *PoolSavings {
	ps := &PoolSavings{
		PoolID:        poolID,
		Zone:          zone,
		NodeSavings:   nodes,
		PoolAction:    poolAction,
		PoolRiskScore: poolRisk,
	}

	for _, ns := range nodes {
		ps.TotalNodes++
		ps.CurrentCostHourly += ns.CurrentCostHourly

		if ns.IsSpot {
			ps.SpotNodes++
			ps.OptimalCostHourly += ns.SpotPriceHourly
		} else {
			ps.ODNodes++
			if ns.CanMigrate {
				ps.OptimizableOD++
				ps.OptimalCostHourly += ns.SpotPriceHourly // Could be spot
				ps.PotentialSavingsHour += ns.SavingsHourly
			} else {
				ps.OptimalCostHourly += ns.ODPriceHourly // Keep on OD
			}
		}
	}

	ps.PotentialSavingsDay = ps.PotentialSavingsHour * 24
	ps.PotentialSavingsMonth = ps.PotentialSavingsDay * 30

	return ps
}

// emitDryRunMetrics publishes savings data to Prometheus for Grafana dashboards.
func emitDryRunMetrics(report *SavingsReport) {
	for poolID, ps := range report.Pools {
		// Pool-level metrics
		metrics.PotentialSavingsPoolTotal.WithLabelValues(poolID).Set(ps.PotentialSavingsMonth)
		metrics.NodesOptimizable.WithLabelValues(poolID).Set(float64(ps.OptimizableOD))
		metrics.RiskScore.WithLabelValues(poolID, ps.Zone).Set(float64(ps.PoolRiskScore))

		// Spot ratio metrics
		if ps.TotalNodes > 0 {
			currentRatio := float64(ps.SpotNodes) / float64(ps.TotalNodes)
			metrics.SpotRatioCurrent.WithLabelValues(poolID).Set(currentRatio)
		}

		// Node-level metrics
		for _, ns := range ps.NodeSavings {
			metrics.PotentialSavingsHourly.WithLabelValues(ns.NodeID, poolID, ns.InstanceType).Set(ns.SavingsHourly)
			metrics.RecommendedAction.WithLabelValues(ns.NodeID, poolID).Set(float64(ns.Action))
		}
	}

	// Cumulative counter for dry-run value tracking
	if report.TotalSavingsHour > 0 {
		metrics.DryRunCumulativeSavings.Add(report.TotalSavingsHour)
	}
}

// logDryRunReport outputs the savings report in a customer-friendly format.
func logDryRunReport(logger *slog.Logger, report *SavingsReport) {
	logger.Info("=== SpotVortex Dry-Run Savings Report ===")

	for poolID, ps := range report.Pools {
		logger.Info("Pool Summary",
			"pool", poolID,
			"zone", ps.Zone,
			"total_nodes", ps.TotalNodes,
			"spot_nodes", ps.SpotNodes,
			"od_nodes", ps.ODNodes,
			"optimizable_od", ps.OptimizableOD,
			"risk_score", fmt.Sprintf("%.2f", ps.PoolRiskScore),
			"action", inference.ActionToString(ps.PoolAction),
		)

		if ps.PotentialSavingsMonth > 0 {
			logger.Info("Pool Potential Savings",
				"pool", poolID,
				"savings_hourly", fmt.Sprintf("$%.3f", ps.PotentialSavingsHour),
				"savings_daily", fmt.Sprintf("$%.2f", ps.PotentialSavingsDay),
				"savings_monthly", fmt.Sprintf("$%.2f", ps.PotentialSavingsMonth),
			)
		}

		// Log individual node recommendations
		for _, ns := range ps.NodeSavings {
			if ns.CanMigrate || ns.RiskLevel == "high" {
				logger.Info("[DRY-RUN] Node Recommendation",
					"node", ns.NodeID,
					"instance_type", ns.InstanceType,
					"is_spot", ns.IsSpot,
					"current_cost_hr", fmt.Sprintf("$%.3f", ns.CurrentCostHourly),
					"spot_price_hr", fmt.Sprintf("$%.3f", ns.SpotPriceHourly),
					"od_price_hr", fmt.Sprintf("$%.3f", ns.ODPriceHourly),
					"potential_savings_hr", fmt.Sprintf("$%.3f", ns.SavingsHourly),
					"potential_savings_mo", fmt.Sprintf("$%.2f", ns.SavingsMonthly),
					"risk_level", ns.RiskLevel,
					"action", inference.ActionToString(ns.Action),
					"recommendation", ns.Recommendation,
				)
			}
		}
	}

	// Total summary
	logger.Info("=== Total Potential Savings ===",
		"total_nodes", report.TotalNodes,
		"optimizable_nodes", report.OptimizableNodes,
		"total_savings_hourly", fmt.Sprintf("$%.3f", report.TotalSavingsHour),
		"total_savings_daily", fmt.Sprintf("$%.2f", report.TotalSavingsDay),
		"total_savings_monthly", fmt.Sprintf("$%.2f", report.TotalSavingsMonth),
	)
}

// buildSavingsReport constructs a complete savings report from pool aggregations.
// Called during reconciliation when in dry-run mode.
func (c *Controller) buildSavingsReport(ctx context.Context, poolAggregations map[string]*poolAggregation, poolActions map[string]NodeAssessment, priceCache map[string]float64) *SavingsReport {
	report := &SavingsReport{
		Pools: make(map[string]*PoolSavings),
	}

	nodeInfo, _ := c.nodeInfoMap(ctx)

	for poolKey, agg := range poolAggregations {
		poolAction, ok := poolActions[poolKey]
		if !ok {
			continue
		}

		var nodeSavings []NodeSavings
		for _, m := range agg.nodes {
			nodeID := m.NodeID
			instanceType := m.InstanceType
			zone := m.Zone
			isSpot := m.IsSpot

			// Get node info if available
			if info, ok := nodeInfo[nodeID]; ok {
				if instanceType == "" {
					instanceType = info.instanceType
				}
				if zone == "" {
					zone = info.zone
				}
				isSpot = info.isSpot
			}

			// Get prices from cache or use defaults
			spotPrice := priceCache[instanceType+":"+zone+":spot"]
			odPrice := priceCache[instanceType+":"+zone+":od"]
			if spotPrice == 0 {
				spotPrice = priceCache[agg.dominantType+":"+zone+":spot"]
			}
			if odPrice == 0 {
				odPrice = priceCache[agg.dominantType+":"+zone+":od"]
			}

			ns := calculateNodeSavings(
				nodeID, poolKey, instanceType, zone,
				isSpot, spotPrice, odPrice,
				poolAction.Action, poolAction.CapacityScore, poolAction.Confidence,
			)
			nodeSavings = append(nodeSavings, ns)
		}

		ps := aggregatePoolSavings(poolKey, agg.zone, nodeSavings, poolAction.Action, poolAction.CapacityScore)
		report.Pools[poolKey] = ps

		report.TotalNodes += ps.TotalNodes
		report.OptimizableNodes += ps.OptimizableOD
		report.TotalSavingsHour += ps.PotentialSavingsHour
	}

	report.TotalSavingsDay = report.TotalSavingsHour * 24
	report.TotalSavingsMonth = report.TotalSavingsDay * 30

	return report
}
