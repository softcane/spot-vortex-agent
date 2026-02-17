// Package main is the entry point for the SpotVortex Agent.
// The agent predicts Spot Instance interruptions and proactively migrates workloads.
package main

import (
	"os"

	"github.com/pradeepsingh/spot-vortex-agent/cmd/agent/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
