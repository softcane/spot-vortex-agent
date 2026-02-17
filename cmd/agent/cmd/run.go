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

	"github.com/softcane/spot-vortex-agent/internal/capacity"
	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/controller"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	"github.com/softcane/spot-vortex-agent/internal/karpenter"
	"github.com/softcane/spot-vortex-agent/internal/metrics"
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

		// Validate Karpenter version: require v1, reject v1beta1-only clusters
		if err := karpenter.ValidateKarpenterVersion(ctx, k8sClient, slog.Default()); err != nil {
			return fmt.Errorf("karpenter version check failed: %w", err)
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

	// 5. Initialize Cloud Provider (required for shadow mode)
	var priceProvider cloudapi.PriceProvider
	priceProvider, _, err = cloudapi.NewAutoDetectedPriceProvider(ctx, slog.Default())
	if err != nil {
		slog.Warn("failed to auto-detect cloud provider, attempting AWS fallback", "error", err)
		priceProvider, err = cloudapi.NewAWSPriceProvider(ctx, cfg.AWS.Region, slog.Default())
		if err != nil {
			return fmt.Errorf("failed to initialize price provider (required for shadow mode): %w", err)
		}
	}

	// 5.5. IAM canary: verify credentials work by making a lightweight spot price call.
	// Use first configured AZ or fall back to region + "a". Real AZs are configured
	// in values.yaml under aws.availabilityZones.
	canaryAZ := cfg.AWS.Region + "a"
	if len(cfg.AWS.AvailabilityZones) > 0 {
		canaryAZ = cfg.AWS.AvailabilityZones[0]
	}
	if _, err := priceProvider.GetSpotPrice(ctx, "m5.large", canaryAZ); err != nil {
		slog.Warn("IAM canary failed: spot price query returned an error; "+
			"verify IAM permissions per docs/IAM_PERMISSIONS.md",
			"error", err,
			"region", cfg.AWS.Region,
			"az", canaryAZ,
		)
	} else {
		slog.Info("IAM canary passed: spot price query succeeded", "az", canaryAZ)
	}

	// Create the safety wrapper
	cloudWrapper := cloudapi.NewSpotWrapper(cloudapi.SpotWrapperConfig{
		DryRun: IsDryRun(),
		Logger: slog.Default(),
	})

	// 5.7. Initialize ASG client for Cluster Autoscaler / Managed Nodegroup integration
	var asgClient capacity.ASGClient
	if cfg.Autoscaling.Enabled {
		realASG, asgErr := capacity.NewAWSASGClient(ctx, capacity.AWSASGClientConfig{
			Region:         cfg.AWS.Region,
			PoolTagKey:     cfg.Autoscaling.DiscoveryTags.Pool,
			CapacityTagKey: cfg.Autoscaling.DiscoveryTags.CapacityType,
		})
		if asgErr != nil {
			return fmt.Errorf("failed to initialize ASG client: %w", asgErr)
		}
		asgClient = realASG
		slog.Info("ASG client initialized", "region", cfg.AWS.Region)
	}

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
		Autoscaling:         cfg.Autoscaling,
		ASGClient:           asgClient,
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
