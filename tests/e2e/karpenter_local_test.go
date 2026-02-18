package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/capacity"
	"github.com/softcane/spot-vortex-agent/internal/cloudapi"
	"github.com/softcane/spot-vortex-agent/internal/controller"
	"github.com/softcane/spot-vortex-agent/internal/inference"
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

func TestKarpenterLocal_ModelActionWeightPatchAndDrainScenarios(t *testing.T) {
	requireKarpenterLocalSuite(t)

	dynamicClient := setupDynamicClient(t)
	nodePoolMgr := karpenter.NewNodePoolManager(dynamicClient, slog.Default())

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

	scenarios := []struct {
		name           string
		action         inference.Action
		wantSpotWeight int32
		wantODWeight   int32
	}{
		{
			name:           "increase_30_favors_spot",
			action:         inference.ActionIncrease30,
			wantSpotWeight: 90,
			wantODWeight:   10,
		},
		{
			name:           "decrease_10_favors_on_demand",
			action:         inference.ActionDecrease10,
			wantSpotWeight: 10,
			wantODWeight:   90,
		},
		{
			name:           "emergency_exit_favors_on_demand",
			action:         inference.ActionEmergencyExit,
			wantSpotWeight: 10,
			wantODWeight:   90,
		},
	}

	for i, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			kMgr := capacity.NewKarpenterManager(capacity.KarpenterManagerConfig{
				NodePoolManager:        nodePoolMgr,
				Logger:                 slog.Default(),
				SpotNodePoolSuffix:     "-spot",
				OnDemandNodePoolSuffix: "-od",
				SpotWeight:             90,
				OnDemandWeight:         10,
				CooldownSeconds:        1,
			})

			direction, ok := swapDirectionForModelAction(tc.action)
			if !ok {
				t.Fatalf("action %s does not map to a capacity swap", inference.ActionToString(tc.action))
			}

			swapResult, err := kMgr.PrepareSwap(ctx, capacity.PoolInfo{Name: "general"}, direction)
			if err != nil {
				t.Fatalf("PrepareSwap failed for action=%s: %v", inference.ActionToString(tc.action), err)
			}
			if !swapResult.Ready {
				t.Fatalf("expected Ready=true after weight steering for action=%s", inference.ActionToString(tc.action))
			}

			spotWeight, err := nodePoolMgr.GetWeight(ctx, "general-spot")
			if err != nil {
				t.Fatalf("failed to read general-spot weight: %v", err)
			}
			odWeight, err := nodePoolMgr.GetWeight(ctx, "general-od")
			if err != nil {
				t.Fatalf("failed to read general-od weight: %v", err)
			}

			if spotWeight != tc.wantSpotWeight {
				t.Fatalf("spot weight=%d, want %d for action=%s", spotWeight, tc.wantSpotWeight, inference.ActionToString(tc.action))
			}
			if odWeight != tc.wantODWeight {
				t.Fatalf("on-demand weight=%d, want %d for action=%s", odWeight, tc.wantODWeight, inference.ActionToString(tc.action))
			}

			podName := fmt.Sprintf("shadow-drain-action-%d", i)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns,
					Labels: map[string]string{
						"app": "shadow-drain-action",
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
			if afterNode.Spec.Unschedulable != beforeNode.Spec.Unschedulable {
				t.Fatalf("node unschedulable changed in dry-run (before=%v after=%v)", beforeNode.Spec.Unschedulable, afterNode.Spec.Unschedulable)
			}

			afterPod, err := client.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("test pod missing after dry-run drain: %v", err)
			}
			if afterPod.DeletionTimestamp != nil {
				t.Fatal("test pod was marked for deletion during dry-run drain")
			}
		})
	}
}

func swapDirectionForModelAction(action inference.Action) (capacity.SwapDirection, bool) {
	switch action {
	case inference.ActionIncrease10, inference.ActionIncrease30:
		return capacity.SwapToSpot, true
	case inference.ActionDecrease10, inference.ActionDecrease30, inference.ActionEmergencyExit:
		return capacity.SwapToOnDemand, true
	default:
		return capacity.SwapToOnDemand, false
	}
}

func TestKarpenterLocal_FakePriceProvider_HappyAndEdgeScenarios(t *testing.T) {
	requireKarpenterLocalSuite(t)

	provider, err := cloudapi.NewFakePriceProviderFromJSON(`{
  "default": {"current_price": 0.20, "on_demand_price": 1.00, "price_history": [0.20, 0.20]},
  "series": {
    "m5.large:us-east-1a": [
      {"current_price": 0.25, "on_demand_price": 1.10, "price_history": [0.24, 0.25]},
      {"current_price": 0.95, "on_demand_price": 1.10, "price_history": [0.70, 0.95]},
      {"current_price": 0.30, "on_demand_price": 1.10, "price_history": []}
    ],
    "r6i.large:us-east-1a": [
      {"error": "simulated provider timeout"}
    ],
    "*:us-west-2a": [
      {"current_price": 0.40, "on_demand_price": 1.20, "price_history": [0.39, 0.40]}
    ]
  }
}`)
	if err != nil {
		t.Fatalf("failed to build fake provider: %v", err)
	}

	ctx := context.Background()

	od, err := provider.GetOnDemandPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetOnDemandPrice failed: %v", err)
	}
	if od != 1.10 {
		t.Fatalf("unexpected on-demand price=%v want=1.10", od)
	}

	first, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice first failed: %v", err)
	}
	if first.CurrentPrice != 0.25 || len(first.PriceHistory) != 2 {
		t.Fatalf("unexpected first step: %+v", first)
	}

	second, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice second failed: %v", err)
	}
	if second.CurrentPrice != 0.95 || len(second.PriceHistory) != 2 {
		t.Fatalf("unexpected second step: %+v", second)
	}

	third, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice third failed: %v", err)
	}
	if third.CurrentPrice != 0.30 {
		t.Fatalf("unexpected third step current_price=%v want=0.30", third.CurrentPrice)
	}
	if len(third.PriceHistory) != 0 {
		t.Fatalf("expected explicit empty history on third step, got=%v", third.PriceHistory)
	}

	if _, err := provider.GetSpotPrice(ctx, "r6i.large", "us-east-1a"); err == nil || !strings.Contains(err.Error(), "simulated provider timeout") {
		t.Fatalf("expected injected provider timeout error, got: %v", err)
	}

	wildcardZone, err := provider.GetSpotPrice(ctx, "c6i.large", "us-west-2a")
	if err != nil {
		t.Fatalf("wildcard zone query failed: %v", err)
	}
	if wildcardZone.CurrentPrice != 0.40 || wildcardZone.OnDemandPrice != 1.20 {
		t.Fatalf("unexpected wildcard zone price: %+v", wildcardZone)
	}

	defaultData, err := provider.GetSpotPrice(ctx, "c6i.large", "eu-west-1a")
	if err != nil {
		t.Fatalf("default fallback query failed: %v", err)
	}
	if defaultData.CurrentPrice != 0.20 || defaultData.OnDemandPrice != 1.00 {
		t.Fatalf("unexpected default fallback data: %+v", defaultData)
	}
}

func TestKarpenterLocal_FakePriceProvider_ExhaustionWithoutRepeat(t *testing.T) {
	requireKarpenterLocalSuite(t)

	provider, err := cloudapi.NewFakePriceProviderFromJSON(`{
  "default": {"current_price": 0.20, "on_demand_price": 1.00},
  "series": {
    "m5.large:us-east-1a": [
      {"current_price": 0.22, "on_demand_price": 1.02}
    ]
  },
  "repeat_last": false
}`)
	if err != nil {
		t.Fatalf("failed to build fake provider: %v", err)
	}

	ctx := context.Background()
	if _, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a"); err != nil {
		t.Fatalf("first step should succeed: %v", err)
	}
	if _, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected exhaustion error on second call, got: %v", err)
	}
}
