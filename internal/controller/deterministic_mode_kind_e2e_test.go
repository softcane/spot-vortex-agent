package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pradeepsingh/spot-vortex-agent/internal/inference"
	svmetrics "github.com/pradeepsingh/spot-vortex-agent/internal/metrics"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestDeterministicModeKindInferencePath(t *testing.T) {
	ctxName := currentKubeContext()
	if !strings.HasPrefix(ctxName, "kind-") {
		t.Skipf("skipping Kind deterministic e2e path test, current context=%q", ctxName)
	}
	if _, err := os.Stat("../../models/tft.onnx"); err != nil {
		t.Skip("skipping: ../../models/tft.onnx not found")
	}
	if _, err := os.Stat("../../models/rl_policy.onnx"); err != nil {
		t.Skip("skipping: ../../models/rl_policy.onnx not found")
	}

	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")
	t.Setenv("SPOTVORTEX_PRICE_MODE", "synthetic")
	t.Cleanup(writeRuntimeConfigForTest(t, `{
  "risk_multiplier": 1.0,
  "min_spot_ratio": 0.0,
  "max_spot_ratio": 1.0,
  "target_spot_ratio": 0.5,
  "step_minutes": 30,
  "policy_mode": "deterministic",
  "deterministic_policy": {
    "emergency_risk_threshold": 0.90,
    "runtime_emergency_threshold": 0.80,
    "high_risk_threshold": 0.60,
    "medium_risk_threshold": 0.35,
    "min_savings_ratio_for_increase": 0.15,
    "max_payback_hours_for_increase": 6.0,
    "ood_mode": "conservative",
    "ood_max_risk_for_increase": 0.25,
    "ood_min_savings_ratio_for_increase": 0.25,
    "ood_max_payback_hours_for_increase": 3.0,
    "feature_buckets": {
      "source": "../../config/workload_distributions.yaml"
    }
  }
}`))

	k8sClient, err := kubeClientForTest()
	if err != nil {
		t.Fatalf("failed to build kube client: %v", err)
	}
	nodes, err := k8sClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("no nodes found in Kind cluster")
	}

	infEngine, err := inference.NewInferenceEngine(inference.EngineConfig{
		TFTModelPath:       "../../models/tft.onnx",
		RLModelPath:        "../../models/rl_policy.onnx",
		RequireRuntimeHead: true,
		Logger:             slog.Default(),
	})
	if err != nil {
		t.Fatalf("failed to initialize inference engine: %v", err)
	}
	t.Cleanup(func() {
		infEngine.Close()
	})

	c, err := New(Config{
		Cloud:               &noopCloudProvider{dryRun: true},
		K8sClient:           k8sClient,
		Inference:           infEngine,
		PrometheusClient:    &svmetrics.Client{},
		Logger:              slog.Default(),
		RiskThreshold:       0.6,
		MaxDrainRatio:       0.2,
		ReconcileInterval:   10 * time.Second,
		ConfidenceThreshold: 0.5,
	})
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	nodeMetrics, err := c.fetchNodeMetrics(context.Background())
	if err != nil {
		t.Fatalf("failed to fetch synthetic node metrics: %v", err)
	}
	if len(nodeMetrics) == 0 {
		t.Fatal("synthetic node metrics are empty")
	}
	if _, err := c.coll.Collect(context.Background()); err != nil {
		t.Fatalf("failed to collect workload metrics: %v", err)
	}

	beforeDeterministic := decisionSourceTotal("deterministic")
	assessments, err := c.runInference(context.Background(), nodeMetrics)
	if err != nil {
		t.Fatalf("runInference failed: %v", err)
	}
	if len(assessments) == 0 {
		t.Fatal("runInference returned no assessments")
	}

	afterDeterministic := decisionSourceTotal("deterministic")
	if afterDeterministic-beforeDeterministic <= 0 {
		t.Fatalf("expected deterministic decision_source counter to increase (before=%.0f after=%.0f)", beforeDeterministic, afterDeterministic)
	}

	for _, a := range assessments {
		if a.Confidence != 1.0 {
			t.Fatalf("expected deterministic override confidence=1.0, got %.3f", a.Confidence)
		}
		if inference.ActionToString(a.Action) == "UNKNOWN" {
			t.Fatalf("received unknown action for node %s", a.NodeID)
		}
	}
}

func writeRuntimeConfigForTest(t *testing.T, content string) func() {
	t.Helper()

	runtimePath := filepath.Join("config", "runtime.json")
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
		t.Fatalf("failed to create test runtime dir: %v", err)
	}

	var original []byte
	hadOriginal := false
	if existing, err := os.ReadFile(runtimePath); err == nil {
		original = existing
		hadOriginal = true
	}

	if err := os.WriteFile(runtimePath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test runtime config: %v", err)
	}

	return func() {
		if hadOriginal {
			_ = os.WriteFile(runtimePath, original, 0o644)
			return
		}
		_ = os.Remove(runtimePath)
		_ = os.Remove(filepath.Dir(runtimePath))
	}
}

func decisionSourceTotal(source string) float64 {
	actions := []inference.Action{
		inference.ActionHold,
		inference.ActionDecrease10,
		inference.ActionDecrease30,
		inference.ActionIncrease10,
		inference.ActionIncrease30,
		inference.ActionEmergencyExit,
	}
	total := 0.0
	for _, action := range actions {
		counter, err := svmetrics.DecisionSource.GetMetricWithLabelValues(source, inference.ActionToString(action))
		if err != nil {
			continue
		}
		m := &dto.Metric{}
		if err := counter.Write(m); err != nil {
			continue
		}
		total += m.GetCounter().GetValue()
	}
	return total
}

func currentKubeContext() string {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.CurrentContext)
}

func kubeClientForTest() (*kubernetes.Clientset, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build kube config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create kube clientset: %w", err)
	}
	return clientset, nil
}
