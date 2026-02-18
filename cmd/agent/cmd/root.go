// Package cmd provides the CLI commands for the SpotVortex Agent.
package cmd

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	// Global flags
	dryRun  bool
	verbose bool
	cfgFile string
)

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "agent",
	Short: "SpotVortex Agent - Intelligent Spot Instance Management",
	Long: `SpotVortex Agent predicts cloud Spot Instance interruptions using
deep learning and proactively migrates workloads to maintain 100% uptime.

Prime Directive: Uptime over Cost. If uncertain, we fall back to On-Demand.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return setupLogging()
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	// Global persistent flags
	rootCmd.PersistentFlags().BoolVarP(&dryRun, "dry-run", "n", true,
		"Shadow mode: log actions without executing them (default: true, set --dry-run=false for active mode)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false,
		"Enable verbose logging output")
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"Path to configuration file (default: $HOME/.spotvortex.yaml)")
}

// setupLogging configures structured JSON logging using slog.
func setupLogging() error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	handler := slog.NewJSONHandler(os.Stdout, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	if dryRun {
		slog.Info(
			"dry-run mode enabled",
			"action", "mutating cloud actions are disabled; read-only market/telemetry calls may still occur",
		)
	}

	return nil
}

// IsDryRun returns whether dry-run mode is enabled.
func IsDryRun() bool {
	return dryRun
}
