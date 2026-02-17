package e2e

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/capacity"
	"github.com/softcane/spot-vortex-agent/internal/controller"
	"github.com/softcane/spot-vortex-agent/internal/karpenter"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func requireKarpenterLocalSuite(t *testing.T) {
	t.Helper()
	if os.Getenv(e2eSuiteEnv) != "karpenter-local" {
		t.Skipf("set %s=karpenter-local to run this suite", e2eSuiteEnv)
	}
}

func setupDynamicClient(t *testing.T) dynamic.Interface {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", getKubeconfig())
	if err != nil {
		t.Fatalf("failed to build kubeconfig for dynamic client: %v", err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create dynamic client: %v", err)
	}
	return dc
}

func pickWorkerNode(t *testing.T) string {
	t.Helper()
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list nodes: %v", err)
	}

	for _, node := range nodes.Items {
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			continue
		}
		return node.Name
	}
	t.Fatal("no worker node found")
	return ""
}

func TestKarpenterLocal_WeightSteeringPatchesNodePools(t *testing.T) {
	requireKarpenterLocalSuite(t)

	dynamicClient := setupDynamicClient(t)
	nodePoolMgr := karpenter.NewNodePoolManager(dynamicClient, slog.Default())
	kMgr := capacity.NewKarpenterManager(capacity.KarpenterManagerConfig{
		NodePoolManager:        nodePoolMgr,
		Logger:                 slog.Default(),
		SpotNodePoolSuffix:     "-spot",
		OnDemandNodePoolSuffix: "-od",
		SpotWeight:             90,
		OnDemandWeight:         10,
		CooldownSeconds:        1,
	})

	ctx := context.Background()
	result, err := kMgr.PrepareSwap(ctx, capacity.PoolInfo{Name: "general"}, capacity.SwapToOnDemand)
	if err != nil {
		t.Fatalf("PrepareSwap failed: %v", err)
	}
	if !result.Ready {
		t.Fatal("expected Ready=true after NodePool patching")
	}

	spotWeight, err := nodePoolMgr.GetWeight(ctx, "general-spot")
	if err != nil {
		t.Fatalf("failed to read general-spot weight: %v", err)
	}
	odWeight, err := nodePoolMgr.GetWeight(ctx, "general-od")
	if err != nil {
		t.Fatalf("failed to read general-od weight: %v", err)
	}

	if spotWeight != 10 {
		t.Fatalf("spot weight=%d, want 10 after SwapToOnDemand", spotWeight)
	}
	if odWeight != 90 {
		t.Fatalf("on-demand weight=%d, want 90 after SwapToOnDemand", odWeight)
	}
}

func TestKarpenterLocal_ShadowDrainDoesNotEvict(t *testing.T) {
	requireKarpenterLocalSuite(t)

	client := setupClient(t)
	ctx := context.Background()
	nodeName := pickWorkerNode(t)

	const ns = "spotvortex-e2e"
	if _, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			if _, createErr := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}, metav1.CreateOptions{}); createErr != nil {
				t.Fatalf("failed to create namespace %s: %v", ns, createErr)
			}
		} else {
			t.Fatalf("failed to get namespace %s: %v", ns, err)
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shadow-drain-pod",
			Namespace: ns,
			Labels: map[string]string{
				"app": "shadow-drain",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.9",
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}

	if _, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("failed to create test pod: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Pods(ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: func() *int64 { v := int64(0); return &v }(),
		})
	})

	beforeNode, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node before drain: %v", err)
	}
	wasUnschedulable := beforeNode.Spec.Unschedulable

	drainer := controller.NewDrainer(client, slog.Default(), controller.DrainConfig{
		DryRun:             true,
		GracePeriodSeconds: 1,
		Timeout:            2 * time.Minute,
		IgnoreDaemonSets:   true,
		DeleteEmptyDirData: true,
	})

	result, err := drainer.Drain(ctx, nodeName)
	if err != nil {
		t.Fatalf("dry-run drain failed: %v", err)
	}
	if !result.DryRun {
		t.Fatal("expected dry-run drain result")
	}

	afterNode, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node after drain: %v", err)
	}
	if afterNode.Spec.Unschedulable != wasUnschedulable {
		t.Fatalf("node unschedulable changed in dry-run (before=%v after=%v)", wasUnschedulable, afterNode.Spec.Unschedulable)
	}

	afterPod, err := client.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("test pod missing after dry-run drain: %v", err)
	}
	if afterPod.DeletionTimestamp != nil {
		t.Fatal("test pod was marked for deletion during dry-run drain")
	}
}
