package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/config"
	"github.com/softcane/spot-vortex-agent/internal/inference"
	svmetrics "github.com/softcane/spot-vortex-agent/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestDeterministicModeKindInferencePath(t *testing.T) {
	if _, err := os.Stat("../../models/tft.onnx"); err != nil {
		t.Skip("skipping: ../../models/tft.onnx not found")
	}
	if _, err := os.Stat("../../models/rl_policy.onnx"); err != nil {
		t.Skip("skipping: ../../models/rl_policy.onnx not found")
	}
	if _, err := os.Stat("../../models/MODEL_MANIFEST.json"); err != nil {
		t.Skip("skipping: ../../models/MODEL_MANIFEST.json not found")
	}

	kubeconfigPath := ensureKindClusterForTest(t)
	t.Setenv("KUBECONFIG", kubeconfigPath)

	t.Setenv("SPOTVORTEX_METRICS_MODE", "synthetic")
	t.Cleanup(stageShippedRuntimeConfigForTest(t))

	k8sClient, err := kubeClientForTest()
	if err != nil {
		t.Fatalf("failed to build kube client: %v", err)
	}
	nodes, err := k8sClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list nodes from ensured Kind cluster: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("no nodes found in Kind cluster")
	}

	infEngine, err := inference.NewInferenceEngine(inference.EngineConfig{
		TFTModelPath:         "../../models/tft.onnx",
		RLModelPath:          "../../models/rl_policy.onnx",
		ModelManifestPath:    "../../models/MODEL_MANIFEST.json",
		ExpectedCloud:        "aws",
		RequireModelContract: true,
		Logger:               slog.Default(),
	})
	if err != nil {
		if strings.Contains(err.Error(), "failed to initialize ONNX runtime") ||
			strings.Contains(err.Error(), "InitializeRuntime() has either not yet been called") ||
			strings.Contains(err.Error(), "Error loading ONNX shared library") {
			t.Skipf("skipping deterministic e2e path test: onnxruntime unavailable: %v", err)
		}
		t.Fatalf("failed to initialize inference engine: %v", err)
	}
	t.Cleanup(func() {
		infEngine.Close()
	})

	c, err := New(Config{
		Cloud: &noopCloudProvider{dryRun: true},
		PriceProvider: &MockPriceProvider{
			PriceData: cloudapi.SpotPriceData{
				CurrentPrice:  0.2,
				OnDemandPrice: 1.0,
				PriceHistory:  []float64{0.2, 0.2, 0.2},
			},
		},
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
	beforeRL := decisionSourceTotal("rl")
	beforeShadowRecommended := counterValueTwoLabels(t, svmetrics.ShadowActionRecommended, "rl", "HOLD")
	beforeShadowSame := counterValueOneLabel(t, svmetrics.ShadowActionAgreement, "same")
	beforeShadowDifferent := counterValueOneLabel(t, svmetrics.ShadowActionAgreement, "different")
	assessments, err := c.runInference(context.Background(), nodeMetrics)
	if err != nil {
		t.Fatalf("runInference failed: %v", err)
	}
	if len(assessments) == 0 {
		t.Fatal("runInference returned no assessments")
	}

	shadowRecorded := false
	for _, a := range assessments {
		if a.Confidence != 1.0 {
			t.Fatalf("expected deterministic override confidence=1.0, got %.3f", a.Confidence)
		}
		if inference.ActionToString(a.Action) == "UNKNOWN" {
			t.Fatalf("received unknown action for node %s", a.NodeID)
		}
		if a.HasShadow {
			shadowRecorded = true
		}
	}
	if !shadowRecorded {
		t.Skip("skipping: current Kind cluster nodes are outside model scope, so deterministic+shadow comparison path was not exercised")
	}

	afterDeterministic := decisionSourceTotal("deterministic")
	if afterDeterministic-beforeDeterministic <= 0 {
		t.Fatalf("expected deterministic decision_source counter to increase (before=%.0f after=%.0f)", beforeDeterministic, afterDeterministic)
	}
	afterRL := decisionSourceTotal("rl")
	if afterRL-beforeRL != 0 {
		t.Fatalf("expected no RL actuation decision_source increments in deterministic mode (before=%.0f after=%.0f)", beforeRL, afterRL)
	}

	afterShadowRecommended := counterValueTwoLabels(t, svmetrics.ShadowActionRecommended, "rl", "HOLD")
	afterShadowSame := counterValueOneLabel(t, svmetrics.ShadowActionAgreement, "same")
	afterShadowDifferent := counterValueOneLabel(t, svmetrics.ShadowActionAgreement, "different")
	if (afterShadowRecommended-beforeShadowRecommended)+(afterShadowSame-beforeShadowSame)+(afterShadowDifferent-beforeShadowDifferent) <= 0 {
		t.Fatal("expected RL shadow metrics to increase after deterministic runInference")
	}

	metricsText := scrapeMetricsText(t)
	requiredNames := []string{
		"spotvortex_shadow_action_recommended_total",
		"spotvortex_shadow_action_agreement_total",
		"spotvortex_shadow_action_delta_total",
		"spotvortex_shadow_projected_savings_delta_usd",
		"spotvortex_shadow_guardrail_blocked_total",
		"spotvortex_aws_interruption_notice_total",
		"spotvortex_aws_rebalance_recommendation_total",
		"spotvortex_node_termination_total",
		"spotvortex_node_notready_total",
		"spotvortex_pod_evictions_total",
		"spotvortex_pod_restarts_total",
		"spotvortex_pod_pending_duration_seconds",
		"spotvortex_recovery_time_seconds",
	}
	for _, name := range requiredNames {
		if !strings.Contains(metricsText, name) {
			t.Fatalf("metrics scrape missing %q", name)
		}
	}
}

func stageShippedRuntimeConfigForTest(t *testing.T) func() {
	t.Helper()

	runtimePath := filepath.Clean(filepath.Join("..", "..", "config", "runtime.json"))
	cfg, err := config.LoadRuntimeConfig(runtimePath)
	if err != nil {
		t.Fatalf("failed to load shipped runtime config %s: %v", runtimePath, err)
	}
	if cfg.PolicyMode != config.PolicyModeDeterministic {
		t.Fatalf("shipped runtime policy_mode=%q, want %q", cfg.PolicyMode, config.PolicyModeDeterministic)
	}
	if cfg.StepMinutes != 10 {
		t.Fatalf("shipped runtime step_minutes=%d, want 10", cfg.StepMinutes)
	}
	if cfg.MinSpotRatio != 0.167 {
		t.Fatalf("shipped runtime min_spot_ratio=%.3f, want 0.167", cfg.MinSpotRatio)
	}
	if cfg.MaxSpotRatio != 1.0 {
		t.Fatalf("shipped runtime max_spot_ratio=%.3f, want 1.0", cfg.MaxSpotRatio)
	}

	payload, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatalf("failed to read shipped runtime config %s: %v", runtimePath, err)
	}
	return writeRuntimeConfigForTest(t, string(payload))
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

func counterValueOneLabel(t *testing.T, cv *prometheus.CounterVec, label string) float64 {
	t.Helper()
	counter, err := cv.GetMetricWithLabelValues(label)
	if err != nil {
		return 0
	}
	m := &dto.Metric{}
	if err := counter.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func counterValueTwoLabels(t *testing.T, cv *prometheus.CounterVec, first, second string) float64 {
	t.Helper()
	counter, err := cv.GetMetricWithLabelValues(first, second)
	if err != nil {
		return 0
	}
	m := &dto.Metric{}
	if err := counter.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func scrapeMetricsText(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body failed: %v", err)
	}
	return string(body)
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

func ensureKindClusterForTest(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("kind"); err != nil {
		t.Skipf("skipping Kind deterministic e2e path test: kind not installed: %v", err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("skipping Kind deterministic e2e path test: docker not installed: %v", err)
	}
	if err := runTestCommand("", "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Skipf("skipping Kind deterministic e2e path test: docker unavailable: %v", err)
	}

	clusterName := strings.TrimSpace(os.Getenv("SPOTVORTEX_KIND_CLUSTER_NAME"))
	if clusterName == "" {
		clusterName = "spotvortex-controller-e2e"
	}
	kubeconfigPath := filepath.Join(t.TempDir(), "kind.kubeconfig")
	configPath := prepareKindConfigForTest(t)

	reachable := false
	if kindClusterExists(t, clusterName) {
		if err := writeKindKubeconfig(clusterName, kubeconfigPath); err == nil {
			if err := waitForReadyKindNodes(kubeconfigPath, 45*time.Second); err == nil {
				reachable = true
			}
		}
	}

	if !reachable {
		if kindClusterExists(t, clusterName) {
			if err := runTestCommand("", "kind", "delete", "cluster", "--name", clusterName); err != nil {
				t.Fatalf("failed to delete stale Kind cluster %q: %v", clusterName, err)
			}
		}
		if err := runTestCommand("", "kind", "create", "cluster",
			"--name", clusterName,
			"--config", configPath,
			"--kubeconfig", kubeconfigPath,
			"--wait", "120s",
		); err != nil {
			t.Fatalf("failed to create Kind cluster %q: %v", clusterName, err)
		}
		if err := waitForReadyKindNodes(kubeconfigPath, 120*time.Second); err != nil {
			t.Fatalf("Kind cluster %q did not become ready: %v", clusterName, err)
		}

		keepCluster := strings.EqualFold(os.Getenv("SPOTVORTEX_KEEP_KIND_CLUSTER"), "true") ||
			os.Getenv("SPOTVORTEX_KEEP_KIND_CLUSTER") == "1"
		if !keepCluster {
			t.Cleanup(func() {
				_ = runTestCommand("", "kind", "delete", "cluster", "--name", clusterName)
			})
		}
	}

	return kubeconfigPath
}

func prepareKindConfigForTest(t *testing.T) string {
	t.Helper()

	basePath := filepath.Clean(filepath.Join("..", "..", "tests", "e2e", "kind-config.yaml"))
	content, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("failed to read kind config %q: %v", basePath, err)
	}

	// The deterministic controller e2e path only needs a labeled Kind cluster.
	// It does not require the monitoring NodePort, so strip the fixed host port
	// mapping to avoid brittle collisions with other local services and tests.
	sanitized := strings.ReplaceAll(
		string(content),
		"    extraPortMappings:\n      - containerPort: 30000\n        hostPort: 30000\n        protocol: TCP\n",
		"",
	)

	configPath := filepath.Join(t.TempDir(), "kind-config.yaml")
	if err := os.WriteFile(configPath, []byte(sanitized), 0o644); err != nil {
		t.Fatalf("failed to write sanitized kind config: %v", err)
	}
	return configPath
}

func kindClusterExists(t *testing.T, clusterName string) bool {
	t.Helper()

	cmd := exec.Command("kind", "get", "clusters")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == clusterName {
			return true
		}
	}
	return false
}

func writeKindKubeconfig(clusterName, kubeconfigPath string) error {
	cmd := exec.Command("kind", "get", "kubeconfig", "--name", clusterName)
	output, err := cmd.Output()
	if err != nil {
		return err
	}
	return os.WriteFile(kubeconfigPath, output, 0o600)
}

func waitForReadyKindNodes(kubeconfigPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		clientset, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}

		nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		if len(nodes.Items) < 3 {
			lastErr = fmt.Errorf("expected at least 3 nodes, got %d", len(nodes.Items))
			time.Sleep(2 * time.Second)
			continue
		}

		readyNodes := 0
		for _, node := range nodes.Items {
			for _, cond := range node.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					readyNodes++
					break
				}
			}
		}
		if readyNodes == len(nodes.Items) {
			return nil
		}

		lastErr = fmt.Errorf("ready nodes %d/%d", readyNodes, len(nodes.Items))
		time.Sleep(2 * time.Second)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for Kind nodes")
	}
	return lastErr
}

func runTestCommand(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimmed)
	}
	return nil
}
