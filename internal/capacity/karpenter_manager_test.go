package capacity

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/softcane/spot-vortex-agent/internal/karpenter"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func makeNodePool(name string, weight int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"weight": weight,
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"requirements": []interface{}{
							map[string]interface{}{
								"key":      "karpenter.sh/capacity-type",
								"operator": "In",
								"values":   []interface{}{"spot", "on-demand"},
							},
						},
					},
				},
			},
		},
	}
}

func TestKarpenterManager_PrepareSwap_MissingNodePools(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	nodePoolMgr := karpenter.NewNodePoolManager(dyn, slog.Default())
	mgr := NewKarpenterManager(KarpenterManagerConfig{
		NodePoolManager:        nodePoolMgr,
		Logger:                 slog.Default(),
		SpotNodePoolSuffix:     "-spot",
		OnDemandNodePoolSuffix: "-od",
		SpotWeight:             80,
		OnDemandWeight:         20,
		CooldownSeconds:        1,
	})

	result, err := mgr.PrepareSwap(context.Background(), PoolInfo{Name: "missing"}, SwapToOnDemand)
	if err != nil {
		t.Fatalf("PrepareSwap returned unexpected error: %v", err)
	}
	if result.Ready {
		t.Fatal("expected Ready=false when both NodePools are missing")
	}
}

func TestKarpenterManager_PrepareSwap_PatchDenied(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(
		scheme,
		makeNodePool("general-spot", 80),
		makeNodePool("general-od", 20),
	)

	// Inject API authorization failure for NodePool patches.
	dyn.PrependReactor("patch", "nodepools", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "karpenter.sh", Resource: "nodepools"},
			"",
			errors.New("patch denied by webhook"),
		)
	})

	nodePoolMgr := karpenter.NewNodePoolManager(dyn, slog.Default())
	mgr := NewKarpenterManager(KarpenterManagerConfig{
		NodePoolManager:        nodePoolMgr,
		Logger:                 slog.Default(),
		SpotNodePoolSuffix:     "-spot",
		OnDemandNodePoolSuffix: "-od",
		SpotWeight:             80,
		OnDemandWeight:         20,
		CooldownSeconds:        1,
	})

	result, err := mgr.PrepareSwap(context.Background(), PoolInfo{Name: "general"}, SwapToOnDemand)
	if err != nil {
		t.Fatalf("PrepareSwap returned unexpected error: %v", err)
	}
	if result.Ready {
		t.Fatal("expected Ready=false when NodePool patches are forbidden")
	}

	// Ensure original weights were not updated.
	spot, err := nodePoolMgr.GetWeight(context.Background(), "general-spot")
	if err != nil {
		t.Fatalf("GetWeight spot failed: %v", err)
	}
	od, err := nodePoolMgr.GetWeight(context.Background(), "general-od")
	if err != nil {
		t.Fatalf("GetWeight od failed: %v", err)
	}
	if spot != 80 || od != 20 {
		t.Fatalf("expected weights unchanged at spot=80/od=20, got spot=%d od=%d", spot, od)
	}
}

func TestKarpenterManager_PrepareSwap_PartialPatchSuccessStillReady(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(
		scheme,
		makeNodePool("general-spot", 80),
		makeNodePool("general-od", 20),
	)

	// Deny only the on-demand pool patch, allow spot pool patch.
	dyn.PrependReactor("patch", "nodepools", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if !ok {
			return false, nil, nil
		}
		if patchAction.GetName() != "general-od" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "karpenter.sh", Resource: "nodepools"},
			patchAction.GetName(),
			errors.New("od pool denied"),
		)
	})

	nodePoolMgr := karpenter.NewNodePoolManager(dyn, slog.Default())
	mgr := NewKarpenterManager(KarpenterManagerConfig{
		NodePoolManager:        nodePoolMgr,
		Logger:                 slog.Default(),
		SpotNodePoolSuffix:     "-spot",
		OnDemandNodePoolSuffix: "-od",
		SpotWeight:             80,
		OnDemandWeight:         20,
		CooldownSeconds:        1,
	})

	result, err := mgr.PrepareSwap(context.Background(), PoolInfo{Name: "general"}, SwapToSpot)
	if err != nil {
		t.Fatalf("PrepareSwap returned unexpected error: %v", err)
	}
	if !result.Ready {
		t.Fatal("expected Ready=true when at least one NodePool patch succeeds")
	}

	spot, err := nodePoolMgr.GetWeight(context.Background(), "general-spot")
	if err != nil {
		t.Fatalf("GetWeight spot failed: %v", err)
	}
	od, err := nodePoolMgr.GetWeight(context.Background(), "general-od")
	if err != nil {
		t.Fatalf("GetWeight od failed: %v", err)
	}
	if spot != 80 || od != 20 {
		t.Fatalf("unexpected weights after partial success, got spot=%d od=%d", spot, od)
	}
}
