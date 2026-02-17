package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pradeepsingh/spot-vortex-agent/internal/cloudapi"
	"github.com/pradeepsingh/spot-vortex-agent/internal/config"
	"github.com/pradeepsingh/spot-vortex-agent/internal/controller"
	"github.com/pradeepsingh/spot-vortex-agent/internal/inference"
	"github.com/pradeepsingh/spot-vortex-agent/internal/metrics"
	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the SpotVortex agent controller",
	Long: `Run starts the SpotVortex agent in controller mode.

The agent will:
1. Connect to the Kubernetes API server
2. Subscribe to the Vortex-Brain prediction service
3. Monitor nodes and proactively drain at-risk instances

Use --dry-run to test without affecting the cluster.`,
	RunE: runAgent,
}

func init() {
	rootCmd.AddCommand(runCmd)
}

func runAgent(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	slog.Info("starting SpotVortex agent",
		"dry_run", IsDryRun(),
		"version", "0.1.0",
	)

	// 1. Load Configuration
	if cfgFile == "" {
		cfgFile = "config/default.yaml" // Fallback to local default for now
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	useSyntheticMetrics := strings.EqualFold(os.Getenv("SPOTVORTEX_METRICS_MODE"), "synthetic")
	useSyntheticPrices := strings.EqualFold(os.Getenv("SPOTVORTEX_PRICE_MODE"), "synthetic")
	if err := validateSyntheticModePolicy(IsDryRun(), useSyntheticMetrics, useSyntheticPrices, os.Getenv("SPOTVORTEX_METRICS_MODE"), os.Getenv("SPOTVORTEX_PRICE_MODE")); err != nil {
		return err
	}

	// 2. Initialize Kubernetes Client
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to kubeconfig if not in cluster
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return fmt.Errorf("failed to load kubernetes config: %w", err)
		}
	}
	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// 2.5. Initialize Dynamic Client for Karpenter integration
	var dynamicClient dynamic.Interface
	if cfg.Karpenter.Enabled {
		dynamicClient, err = dynamic.NewForConfig(k8sConfig)
		if err != nil {
			slog.Warn("failed to create dynamic client for Karpenter integration", "error", err)
		} else {
			slog.Info("Karpenter integration enabled")
		}
	}

	// 3. Initialize Prometheus Client
	promClient, err := metrics.NewClient(metrics.ClientConfig{
		PrometheusURL: cfg.Prometheus.URL,
		Logger:        slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("failed to initialize prometheus client: %w", err)
	}

	// 4. Initialize Inference Engine
	infEngine, err := inference.NewInferenceEngine(inference.EngineConfig{
		TFTModelPath:         cfg.Inference.TFTModelPath,
		RLModelPath:          cfg.Inference.RLModelPath,
		ModelManifestPath:    cfg.Inference.ModelManifestPath,
		ExpectedCloud:        cfg.Inference.ExpectedCloud,
		RequireModelContract: true,
		RequireRuntimeHead:   !IsDryRun(),
		Logger:               slog.Default(),
	})
	if err != nil {
		return fmt.Errorf("failed to initialize inference engine: %w", err)
	}
	defer infEngine.Close()

	// 5. Initialize Cloud Provider
	// For production, we use the factory to detect and create the provider
	var priceProvider cloudapi.PriceProvider
	if useSyntheticPrices {
		slog.Warn("synthetic price mode enabled; skipping cloud price provider detection")
	} else {
		priceProvider, _, err = cloudapi.NewAutoDetectedPriceProvider(ctx, slog.Default())
		if err != nil {
			slog.Warn("failed to auto-detect cloud provider, attempting AWS fallback", "error", err)
			priceProvider, err = cloudapi.NewAWSPriceProvider(ctx, cfg.AWS.Region, slog.Default())
			if err != nil {
				slog.Warn("failed to create AWS price provider", "error", err)
			}
		}
	}

	// Create the safety wrapper
	cloudWrapper := cloudapi.NewSpotWrapper(cloudapi.SpotWrapperConfig{
		DryRun: IsDryRun(),
		Logger: slog.Default(),
	})

	// 6. Initialize Controller
	ctrl, err := controller.New(controller.Config{
		Cloud:               cloudWrapper,
		PriceProvider:       priceProvider,
		K8sClient:           k8sClient,
		DynamicClient:       dynamicClient,
		Inference:           infEngine,
		PrometheusClient:    promClient,
		Logger:              slog.Default(),
		RiskThreshold:       cfg.Controller.RiskThreshold,
		MaxDrainRatio:       cfg.Controller.MaxDrainRatio,
		ReconcileInterval:   cfg.Controller.ReconcileInterval(),
		ConfidenceThreshold: cfg.Controller.ConfidenceThreshold,
		Karpenter:           cfg.Karpenter,
	})
	if err != nil {
		return fmt.Errorf("failed to create controller: %w", err)
	}

	slog.Info("agent ready, starting reconciliation loop...")

	// 7. Start Metrics Server (Non-blocking)
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		slog.Info("starting metrics server", "port", 8080)
		if err := http.ListenAndServe(":8080", mux); err != nil {
			slog.Error("metrics server failed", "error", err)
		}
	}()

	// 8. Start the Controller
	if err := ctrl.Start(ctx); err != nil {
		return fmt.Errorf("controller failure: %w", err)
	}

	return nil
}
