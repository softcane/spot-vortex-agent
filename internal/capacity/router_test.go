package capacity

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stubManager is a minimal CapacityManager for router tests.
type stubManager struct {
	mgrType       ManagerType
	prepareCalled bool
	cleanupCalled bool
}

func (s *stubManager) Type() ManagerType { return s.mgrType }
func (s *stubManager) PrepareSwap(ctx context.Context, pool PoolInfo, dir SwapDirection) (*SwapResult, error) {
	s.prepareCalled = true
	return &SwapResult{Ready: true, Duration: time.Millisecond}, nil
}
func (s *stubManager) PostDrainCleanup(ctx context.Context, nodeName string, pool PoolInfo) error {
	s.cleanupCalled = true
	return nil
}
func (s *stubManager) IsAvailable(ctx context.Context) bool { return true }

func TestRouter_ManagerForNode(t *testing.T) {
	kMgr := &stubManager{mgrType: ManagerKarpenter}
	caMgr := &stubManager{mgrType: ManagerClusterAutoscaler}

	router := NewRouter(slog.Default(), kMgr, caMgr)

	tests := []struct {
		name        string
		labels      map[string]string
		expectedMgr ManagerType
		expectedNil bool
	}{
		{
			name:        "karpenter node",
			labels:      map[string]string{"karpenter.sh/nodepool": "pool-spot"},
			expectedMgr: ManagerKarpenter,
		},
		{
			name:        "CA node",
			labels:      map[string]string{"spotvortex.io/manager": "cluster-autoscaler"},
			expectedMgr: ManagerClusterAutoscaler,
		},
		{
			name:        "MNG node falls back to CA manager",
			labels:      map[string]string{"eks.amazonaws.com/nodegroup": "my-ng"},
			expectedMgr: ManagerClusterAutoscaler, // Falls back since no MNG manager registered
		},
		{
			name:        "unknown node returns nil",
			labels:      map[string]string{"app": "nginx"},
			expectedNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: tc.labels},
			}
			mgr := router.ManagerForNode(node)
			if tc.expectedNil {
				if mgr != nil {
					t.Errorf("expected nil, got %v", mgr.Type())
				}
				return
			}
			if mgr == nil {
				t.Fatal("expected non-nil manager")
			}
			if mgr.Type() != tc.expectedMgr {
				t.Errorf("got %q, want %q", mgr.Type(), tc.expectedMgr)
			}
		})
	}
}

func TestRouter_PrepareSwapForNode(t *testing.T) {
	kMgr := &stubManager{mgrType: ManagerKarpenter}
	router := NewRouter(slog.Default(), kMgr)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "karpenter-node",
			Labels: map[string]string{"karpenter.sh/nodepool": "pool"},
		},
	}

	ctx := context.Background()
	result, err := router.PrepareSwapForNode(ctx, node, PoolInfo{Name: "pool"}, SwapToOnDemand)
	if err != nil {
		t.Fatalf("PrepareSwapForNode: %v", err)
	}
	if !result.Ready {
		t.Error("expected Ready=true")
	}
	if !kMgr.prepareCalled {
		t.Error("expected karpenter manager PrepareSwap to be called")
	}
}

func TestRouter_PrepareSwapForNode_UnknownManager(t *testing.T) {
	router := NewRouter(slog.Default()) // no managers registered

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "unknown-node",
			Labels: map[string]string{"app": "nginx"},
		},
	}

	ctx := context.Background()
	_, err := router.PrepareSwapForNode(ctx, node, PoolInfo{Name: "pool"}, SwapToOnDemand)
	if err == nil {
		t.Error("expected error for unknown manager")
	}
}

func TestRouter_RegisteredTypes(t *testing.T) {
	kMgr := &stubManager{mgrType: ManagerKarpenter}
	caMgr := &stubManager{mgrType: ManagerClusterAutoscaler}
	router := NewRouter(slog.Default(), kMgr, caMgr)

	types := router.RegisteredTypes()
	if len(types) != 2 {
		t.Errorf("got %d types, want 2", len(types))
	}

	found := make(map[ManagerType]bool)
	for _, mt := range types {
		found[mt] = true
	}
	if !found[ManagerKarpenter] {
		t.Error("missing karpenter type")
	}
	if !found[ManagerClusterAutoscaler] {
		t.Error("missing CA type")
	}
}

func TestRouter_MNGFallsBackToCA(t *testing.T) {
	// When only CA manager is registered, MNG nodes should route to it
	caMgr := &stubManager{mgrType: ManagerClusterAutoscaler}
	router := NewRouter(slog.Default(), caMgr)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "mng-node",
			Labels: map[string]string{"eks.amazonaws.com/nodegroup": "my-ng"},
		},
	}

	mgr := router.ManagerForNode(node)
	if mgr == nil {
		t.Fatal("expected MNG node to fall back to CA manager")
	}
	if mgr.Type() != ManagerClusterAutoscaler {
		t.Errorf("got %q, want %q", mgr.Type(), ManagerClusterAutoscaler)
	}
}
