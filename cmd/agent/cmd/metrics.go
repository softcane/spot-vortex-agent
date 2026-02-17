package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/softcane/spot-vortex-agent/internal/metrics"
	"github.com/spf13/cobra"
)

var (
	prometheusURL string
	outputFormat  string
)

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Query node metrics from Prometheus",
	Long: `Fetch CPU and memory metrics from a Prometheus server.

This command queries node_exporter metrics to collect CPU and memory
utilization data for all nodes in the cluster.

Example:
  agent metrics --prometheus-url http://localhost:9090
  agent metrics --prometheus-url http://prometheus:9090 --output json`,
	RunE: runMetrics,
}

func init() {
	rootCmd.AddCommand(metricsCmd)

	metricsCmd.Flags().StringVar(&prometheusURL, "prometheus-url", "http://localhost:9090",
		"URL of the Prometheus server")
	metricsCmd.Flags().StringVar(&outputFormat, "output", "table",
		"Output format: table, json")
}

func runMetrics(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	client, err := metrics.NewClient(metrics.ClientConfig{
		PrometheusURL: prometheusURL,
		Logger:        slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("failed to create metrics client: %w", err)
	}

	nodeMetrics, err := client.GetNodeMetrics(ctx)
	if err != nil {
		return fmt.Errorf("failed to get node metrics: %w", err)
	}

	switch outputFormat {
	case "json":
		return outputJSON(nodeMetrics)
	default:
		return outputTable(nodeMetrics)
	}
}

func outputJSON(nodeMetrics []metrics.NodeMetrics) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(nodeMetrics)
}

func outputTable(nodeMetrics []metrics.NodeMetrics) error {
	fmt.Printf("%-30s %-15s %-15s %-10s %-10s\n",
		"NODE", "ZONE", "TYPE", "CPU%", "MEM%")
	fmt.Println("--------------------------------------------------------------------------------")

	for _, m := range nodeMetrics {
		zone := m.Zone
		if zone == "" {
			zone = "-"
		}
		instanceType := m.InstanceType
		if instanceType == "" {
			instanceType = "-"
		}
		fmt.Printf("%-30s %-15s %-15s %-10.1f %-10.1f\n",
			m.NodeID, zone, instanceType, m.CPUUsagePercent, m.MemoryUsagePercent)
	}

	return nil
}
