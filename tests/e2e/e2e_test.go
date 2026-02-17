// Package e2e provides end-to-end integration tests for SpotVortex.
// Uses real ONNX models, real Kind cluster, and real Kubernetes API.
// NO MOCKS.
package e2e

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/softcane/spot-vortex-agent/internal/inference"
	ort "github.com/yalue/onnxruntime_go"
)

var (
	modelsDir  = flag.String("models-dir", "../../models", "Path to ONNX models directory")
	kubeconfig = flag.String("kubeconfig", "", "Path to kubeconfig (defaults to $KUBECONFIG or ~/.kube/config)")
)

func getKubeconfig() string {
	if *kubeconfig != "" {
		return *kubeconfig
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	return filepath.Join(os.Getenv("HOME"), ".kube", "config")
}

func setupClient(t *testing.T) *kubernetes.Clientset {
	config, err := clientcmd.BuildConfigFromFlags("", getKubeconfig())
	if err != nil {
		t.Fatalf("Failed to build kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}

	return clientset
}

// TestONNXModelsLoad verifies ONNX model files exist and are valid.
func TestONNXModelsLoad(t *testing.T) {
	tests := []struct {
		name      string
		modelPath string
	}{
		{
			name:      "TFT Model",
			modelPath: filepath.Join(*modelsDir, "tft.onnx"),
		},
		{
			name:      "RL Policy",
			modelPath: filepath.Join(*modelsDir, "rl_policy.onnx"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, err := os.Stat(tc.modelPath)
			if os.IsNotExist(err) {
				t.Skipf("Model not found: %s", tc.modelPath)
			}
			if err != nil {
				t.Fatalf("Error checking model: %v", err)
			}
			if info.Size() < 100 {
				t.Errorf("Model file too small: %d bytes", info.Size())
			}
			t.Logf("✓ %s exists (%d bytes)", tc.name, info.Size())
		})
	}
}

// TestKindClusterConnection verifies we can connect to Kind cluster.
func TestKindClusterConnection(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	// List nodes
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}

	t.Logf("Found %d nodes", len(nodes.Items))

	// Verify we have expected nodes
	spotNodes := 0
	odNodes := 0
	for _, node := range nodes.Items {
		capacityType := node.Labels["karpenter.sh/capacity-type"]
		switch capacityType {
		case "spot":
			spotNodes++
		case "on-demand":
			odNodes++
		}
		t.Logf("  Node: %s, capacity-type: %s, zone: %s",
			node.Name, capacityType, node.Labels["topology.kubernetes.io/zone"])
	}

	if spotNodes < 3 {
		t.Errorf("Expected at least 3 spot nodes, got %d", spotNodes)
	}
	if odNodes < 1 {
		t.Errorf("Expected at least 1 on-demand node, got %d", odNodes)
	}
}

// TestSpotNodesHaveCorrectLabels verifies spot nodes have SpotVortex labels.
func TestSpotNodesHaveCorrectLabels(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil {
		t.Fatalf("Failed to list spot nodes: %v", err)
	}

	for _, node := range nodes.Items {
		// Check required labels
		requiredLabels := []string{
			"node.kubernetes.io/instance-type",
			"topology.kubernetes.io/zone",
			"karpenter.sh/capacity-type",
			"spotvortex.io/managed",
		}

		for _, label := range requiredLabels {
			if _, ok := node.Labels[label]; !ok {
				t.Errorf("Node %s missing label: %s", node.Name, label)
			}
		}
		t.Logf("✓ Node %s has all required labels", node.Name)
	}
}

// TestPodsHaveSpotVortexAnnotations verifies test pods have correct annotations.
func TestPodsHaveSpotVortexAnnotations(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	expectedAnnotations := map[string][]string{
		"java-service": {"spotvortex.io/outage-penalty", "spotvortex.io/startup-time", "spotvortex.io/critical"},
		"python-api":   {"spotvortex.io/outage-penalty", "spotvortex.io/startup-time"},
		"redis-cache":  {"spotvortex.io/outage-penalty", "spotvortex.io/startup-time"},
	}

	for app, annotations := range expectedAnnotations {
		pods, err := client.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + app,
		})
		if err != nil {
			t.Fatalf("Failed to list pods for %s: %v", app, err)
		}

		if len(pods.Items) == 0 {
			t.Errorf("No pods found for app=%s", app)
			continue
		}

		for _, pod := range pods.Items {
			for _, ann := range annotations {
				if _, ok := pod.Annotations[ann]; !ok {
					t.Errorf("Pod %s missing annotation: %s", pod.Name, ann)
				}
			}
		}
		t.Logf("✓ %s pods have correct annotations", app)
	}
}

// TestPDBsExist verifies PodDisruptionBudgets are created.
func TestPDBsExist(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	expectedPDBs := []string{"java-service-pdb", "redis-cache-pdb"}

	for _, pdbName := range expectedPDBs {
		pdb, err := client.PolicyV1().PodDisruptionBudgets("default").Get(ctx, pdbName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("PDB %s not found: %v", pdbName, err)
			continue
		}
		t.Logf("✓ PDB %s exists (minAvailable=%v, maxUnavailable=%v)",
			pdb.Name, pdb.Spec.MinAvailable, pdb.Spec.MaxUnavailable)
	}
}

// TestFullInferencePipeline tests the complete TFT -> RL -> Action flow.
func TestFullInferencePipeline(t *testing.T) {
	// Initialize ONNX Runtime
	ort.SetSharedLibraryPath(getONNXRuntimeLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		t.Skipf("ONNX Runtime not available: %v", err)
	}
	defer ort.DestroyEnvironment()

	// Step 1: Load TFT model
	tftPath := filepath.Join(*modelsDir, "tft.onnx")
	tftModel, err := inference.NewModel(tftPath, []string{"input"}, []string{"capacity_score", "runtime_score"})
	if err != nil {
		t.Fatalf("Failed to load TFT model: %v", err)
	}
	defer tftModel.Close()
	t.Logf("✓ TFT model loaded from %s", tftPath)

	// Step 2: Load RL model
	rlPath := filepath.Join(*modelsDir, "rl_policy.onnx")
	rlModel, err := inference.NewModel(rlPath, []string{"state"}, []string{"q_values"})
	if err != nil {
		t.Fatalf("Failed to load RL model: %v", err)
	}
	defer rlModel.Close()
	t.Logf("✓ RL model loaded from %s", rlPath)

	// Step 3: Verify Kubernetes cluster access
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}

	t.Logf("✓ Found %d spot nodes for inference", len(nodes.Items))

	fb := inference.NewFeatureBuilder()

	// Step 4: Run real inference for each node
	for _, node := range nodes.Items {
		// Build real state from node + fake price history
		state := inference.NodeState{
			SpotPrice:     0.15,
			OnDemandPrice: 0.85,
			PriceHistory:  []float64{0.15, 0.16, 0.14, 0.15, 0.15, 0.15, 0.17},
			CPUUsage:      0.45,
			MemoryUsage:   0.62,
			IsSpot:        true,
			Timestamp:     time.Now(),
		}

		// TFT Inference (aligned with inference feature schema)
		tftInput, _ := ort.NewTensor(
			ort.NewShape(1, inference.TFTHistorySteps, inference.TFTFeatureCount),
			fb.BuildTFTInput(node.Name, state),
		)
		defer tftInput.Destroy()

		tftResults, err := tftModel.Predict(map[string]*ort.Tensor[float32]{"input": tftInput})
		if err != nil {
			t.Fatalf("TFT inference failed for %s: %v", node.Name, err)
		}
		capacityTensor := tftResults["capacity_score"]
		runtimeTensor := tftResults["runtime_score"]
		if capacityTensor == nil || runtimeTensor == nil {
			t.Fatalf("TFT inference missing dual-head outputs for %s", node.Name)
		}
		capacityScore := float64(capacityTensor.GetData()[0])
		state.RuntimeScore = float64(runtimeTensor.GetData()[0])
		for _, tensor := range tftResults {
			if tensor != nil {
				tensor.Destroy()
			}
		}

		// RL Inference
		rlInput, _ := ort.NewTensor(
			ort.NewShape(1, inference.RLFeatureCount),
			fb.BuildRLInput(state, capacityScore),
		)
		defer rlInput.Destroy()

		rlResults, err := rlModel.Predict(map[string]*ort.Tensor[float32]{"state": rlInput})
		if err != nil {
			t.Fatalf("RL inference failed for %s: %v", node.Name, err)
		}

		// Get best action (argmax)
		qValues := rlResults["q_values"].GetData()
		bestAction := 0
		maxQ := qValues[0]
		for i, qValue := range qValues {
			if qValue > maxQ {
				maxQ = qValue
				bestAction = i
			}
		}
		for _, tensor := range rlResults {
			if tensor != nil {
				tensor.Destroy()
			}
		}

		t.Logf("  Node %s: capacity_score=%.4f action=%d",
			node.Name, capacityScore, bestAction)
	}

	t.Logf("✓ Full inference pipeline test passed (Real ONNX)")
}

// getONNXRuntimeLibPath returns a common path for libonnxruntime.
func getONNXRuntimeLibPath() string {
	if env := os.Getenv("ORT_SHARED_LIBRARY_PATH"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	if env := os.Getenv("SPOTVORTEX_ONNXRUNTIME_PATH"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	patterns := []string{
		".venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		".venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"../../tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"../../tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"../../vortex/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"../../vortex/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, p := range matches {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	paths := []string{
		"/opt/homebrew/lib/libonnxruntime.dylib",
		"/usr/local/lib/libonnxruntime.dylib",
		"/usr/lib/libonnxruntime.so",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "onnxruntime"
}

// TestNodeCordonUncordon verifies we can cordon/uncordon nodes.
func TestNodeCordonUncordon(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	// Get a spot node to test
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("No spot nodes available for cordon test")
	}

	testNode := nodes.Items[0].Name
	t.Logf("Testing cordon/uncordon on node: %s", testNode)

	// Cordon the node
	node, err := client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node: %v", err)
	}

	// Set unschedulable
	node.Spec.Unschedulable = true
	_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to cordon node: %v", err)
	}
	t.Logf("✓ Node %s cordoned", testNode)

	// Verify it's unschedulable
	node, _ = client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
	if !node.Spec.Unschedulable {
		t.Error("Node should be unschedulable after cordon")
	}

	// Uncordon with conflict retry because other controllers can mutate node objects.
	const maxUncordonAttempts = 5
	for attempt := 1; attempt <= maxUncordonAttempts; attempt++ {
		node, err = client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to re-fetch node for uncordon: %v", err)
		}
		node.Spec.Unschedulable = false
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == maxUncordonAttempts {
			t.Fatalf("Failed to uncordon node: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("✓ Node %s uncordoned", testNode)
}

// TestNodeTaintUntaint verifies we can add/remove taints.
func TestNodeTaintUntaint(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("No spot nodes available for taint test")
	}

	testNode := nodes.Items[0].Name
	testTaint := corev1.Taint{
		Key:    "spotvortex.io/draining",
		Effect: corev1.TaintEffectNoSchedule,
	}

	// Add taint with optimistic-lock retry (KWOK nodes are updated frequently).
	maxAttempts := 8
	var node *corev1.Node
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		node, err = client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to fetch node for tainting: %v", err)
		}
		hasTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == testTaint.Key && taint.Effect == testTaint.Effect {
				hasTaint = true
				break
			}
		}
		if !hasTaint {
			node.Spec.Taints = append(node.Spec.Taints, testTaint)
		}
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == maxAttempts {
			t.Fatalf("Failed to add taint: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("✓ Taint added to node %s", testNode)

	// Verify taint exists
	node, err = client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to re-fetch node for taint verification: %v", err)
	}
	hasTaint := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == testTaint.Key {
			hasTaint = true
			break
		}
	}
	if !hasTaint {
		t.Error("Taint should exist on node")
	}

	// Remove taint with optimistic-lock retry.
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		node, err = client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to fetch node for untainting: %v", err)
		}
		var newTaints []corev1.Taint
		for _, taint := range node.Spec.Taints {
			if taint.Key != testTaint.Key {
				newTaints = append(newTaints, taint)
			}
		}
		node.Spec.Taints = newTaints
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == maxAttempts {
			t.Fatalf("Failed to remove taint: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("✓ Taint removed from node %s", testNode)
}

// TestNodeLabelUpdate verifies we can update node labels.
func TestNodeLabelUpdate(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "karpenter.sh/capacity-type=spot",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("No spot nodes available for label test")
	}

	testNode := nodes.Items[0].Name
	testLabel := "spotvortex.io/capacity-score"
	testValue := "0.42"

	// Add label
	node, _ := client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[testLabel] = testValue
	_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update label: %v", err)
	}
	t.Logf("✓ Label %s=%s set on node %s", testLabel, testValue, testNode)

	// Verify label
	node, _ = client.CoreV1().Nodes().Get(ctx, testNode, metav1.GetOptions{})
	if node.Labels[testLabel] != testValue {
		t.Errorf("Label %s should be %s, got %s", testLabel, testValue, node.Labels[testLabel])
	}

	// Cleanup - remove test label
	delete(node.Labels, testLabel)
	_, _ = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	t.Logf("✓ Label %s removed from node %s", testLabel, testNode)
}
