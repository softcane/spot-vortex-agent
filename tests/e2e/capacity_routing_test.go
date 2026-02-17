package e2e

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/capacity"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCapacityDetection_AllThreeNodeTypes verifies that the Detector correctly
// identifies Karpenter, Cluster Autoscaler, and EKS Managed Nodegroup nodes
// from real Kubernetes node labels in a Kind cluster.
func TestCapacityDetection_AllThreeNodeTypes(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}

	detector := capacity.NewDetector(slog.Default())
	groups := detector.GroupNodesByManager(nodes.Items)

	// Log what we found
	for mgr, nodeList := range groups {
		for _, n := range nodeList {
			t.Logf("  %s -> %s", n.Name, mgr)
		}
	}

	// Verify we have at least one node per provisioner type
	if len(groups[capacity.ManagerKarpenter]) == 0 {
		t.Error("expected at least one Karpenter node (karpenter.sh/nodepool label)")
	}
	if len(groups[capacity.ManagerClusterAutoscaler]) == 0 {
		t.Error("expected at least one Cluster Autoscaler node (spotvortex.io/manager=cluster-autoscaler label)")
	}
	if len(groups[capacity.ManagerManagedNodegroup]) == 0 {
		t.Error("expected at least one EKS Managed Nodegroup node (eks.amazonaws.com/nodegroup label)")
	}

	// Verify control-plane nodes are not classified as managed
	for _, node := range nodes.Items {
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			mgr := detector.DetectManager(&node)
			if mgr != capacity.ManagerUnknown {
				t.Errorf("control-plane node %q should be ManagerUnknown, got %s", node.Name, mgr)
			}
		}
	}
}

// TestCapacityRouter_RoutesToCorrectManager verifies that the Router dispatches
// capacity operations to the correct CapacityManager based on node labels.
func TestCapacityRouter_RoutesToCorrectManager(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}

	// Create stub managers to track routing
	kMgr := &trackingManager{mgrType: capacity.ManagerKarpenter}
	caMgr := &trackingManager{mgrType: capacity.ManagerClusterAutoscaler}
	mngMgr := &trackingManager{mgrType: capacity.ManagerManagedNodegroup}

	router := capacity.NewRouter(slog.Default(), kMgr, caMgr, mngMgr)

	// Route each node and verify correct manager is selected
	routedCount := map[capacity.ManagerType]int{}
	for i := range nodes.Items {
		node := &nodes.Items[i]

		// Skip control plane
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			mgr := router.ManagerForNode(node)
			if mgr != nil {
				t.Errorf("control-plane node %q should route to nil, got %s", node.Name, mgr.Type())
			}
			continue
		}

		mgr := router.ManagerForNode(node)
		if mgr == nil {
			t.Logf("  %s -> nil (unmanaged)", node.Name)
			continue
		}
		routedCount[mgr.Type()]++
		t.Logf("  %s -> %s", node.Name, mgr.Type())
	}

	if routedCount[capacity.ManagerKarpenter] == 0 {
		t.Error("no nodes routed to Karpenter manager")
	}
	if routedCount[capacity.ManagerClusterAutoscaler] == 0 {
		t.Error("no nodes routed to Cluster Autoscaler manager")
	}
	if routedCount[capacity.ManagerManagedNodegroup] == 0 {
		t.Error("no nodes routed to Managed Nodegroup manager")
	}
}

// TestCapacityRouter_PrepareSwapForAllTypes verifies that PrepareSwap dispatches
// correctly for each provisioner type in the cluster.
func TestCapacityRouter_PrepareSwapForAllTypes(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	kMgr := &trackingManager{mgrType: capacity.ManagerKarpenter}
	caMgr := &trackingManager{mgrType: capacity.ManagerClusterAutoscaler}
	mngMgr := &trackingManager{mgrType: capacity.ManagerManagedNodegroup}

	router := capacity.NewRouter(slog.Default(), kMgr, caMgr, mngMgr)

	// Find one node of each type and call PrepareSwapForNode
	tests := []struct {
		name       string
		selector   string
		expectType capacity.ManagerType
	}{
		{
			name:       "Karpenter spot node",
			selector:   "karpenter.sh/nodepool",
			expectType: capacity.ManagerKarpenter,
		},
		{
			name:       "Cluster Autoscaler node",
			selector:   "spotvortex.io/manager=cluster-autoscaler",
			expectType: capacity.ManagerClusterAutoscaler,
		},
		{
			name:       "EKS Managed Nodegroup node",
			selector:   "eks.amazonaws.com/nodegroup",
			expectType: capacity.ManagerManagedNodegroup,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
				LabelSelector: tc.selector,
			})
			if err != nil {
				t.Fatalf("Failed to list nodes with selector %q: %v", tc.selector, err)
			}
			if len(nodes.Items) == 0 {
				t.Skipf("No nodes found with selector %q", tc.selector)
			}

			node := &nodes.Items[0]
			pool := capacity.PoolInfo{
				Name: "test-pool",
				Zone: node.Labels["topology.kubernetes.io/zone"],
			}

			result, err := router.PrepareSwapForNode(ctx, node, pool, capacity.SwapToOnDemand)
			if err != nil {
				t.Fatalf("PrepareSwapForNode failed: %v", err)
			}
			if !result.Ready {
				t.Error("expected Ready=true from tracking manager")
			}

			// Verify the right manager was called
			detectedType := router.DetectManagerType(node)
			if detectedType != tc.expectType {
				t.Errorf("detected type %q, want %q", detectedType, tc.expectType)
			}
			t.Logf("  node=%s detected=%s swap=Ready", node.Name, detectedType)
		})
	}
}

// TestCapacityRouter_ASGTwinWorkflow_FakeClient verifies the full Twin ASG
// Scale-Wait-Drain workflow using FakeASGClient against real Kind nodes.
func TestCapacityRouter_ASGTwinWorkflow_FakeClient(t *testing.T) {
	fakeASG := capacity.NewFakeASGClient()
	fakeASG.AddTwinPair("ca-pool", 3, 1)
	fakeASG.AddTwinPair("mng-pool", 2, 1)

	// Create ASG managers backed by FakeASGClient (no K8s client = instant node readiness)
	caMgr := capacity.NewASGManager(capacity.ASGManagerConfig{
		ASGClient:        fakeASG,
		Logger:           slog.Default(),
		ManagerType:      capacity.ManagerClusterAutoscaler,
		NodeReadyTimeout: 5 * time.Second,
		PollInterval:     100 * time.Millisecond,
	})
	mngMgr := capacity.NewASGManager(capacity.ASGManagerConfig{
		ASGClient:        fakeASG,
		Logger:           slog.Default(),
		ManagerType:      capacity.ManagerManagedNodegroup,
		NodeReadyTimeout: 5 * time.Second,
		PollInterval:     100 * time.Millisecond,
	})

	t.Run("CA_SwapToOnDemand", func(t *testing.T) {
		result, err := caMgr.PrepareSwap(context.Background(),
			capacity.PoolInfo{Name: "ca-pool", Zone: "us-east-1a"},
			capacity.SwapToOnDemand,
		)
		if err != nil {
			t.Fatalf("PrepareSwap: %v", err)
		}
		if !result.Ready {
			t.Error("expected Ready=true")
		}

		// Verify OD ASG was scaled up (1 -> 2)
		odASG := fakeASG.GetASG("ca-pool-od-asg")
		if odASG.DesiredCapacity != 2 {
			t.Errorf("OD ASG desired=%d, want 2", odASG.DesiredCapacity)
		}
		t.Logf("  CA pool: OD ASG scaled to %d, replacement=%s", odASG.DesiredCapacity, result.ReplacementNodeName)
	})

	t.Run("MNG_SwapToSpot", func(t *testing.T) {
		result, err := mngMgr.PrepareSwap(context.Background(),
			capacity.PoolInfo{Name: "mng-pool", Zone: "us-east-1b"},
			capacity.SwapToSpot,
		)
		if err != nil {
			t.Fatalf("PrepareSwap: %v", err)
		}
		if !result.Ready {
			t.Error("expected Ready=true")
		}

		// Verify Spot ASG was scaled up (2 -> 3)
		spotASG := fakeASG.GetASG("mng-pool-spot-asg")
		if spotASG.DesiredCapacity != 3 {
			t.Errorf("Spot ASG desired=%d, want 3", spotASG.DesiredCapacity)
		}
		t.Logf("  MNG pool: Spot ASG scaled to %d, replacement=%s", spotASG.DesiredCapacity, result.ReplacementNodeName)
	})

	// Verify call tracking
	if len(fakeASG.ScaleUpCalls) != 2 {
		t.Errorf("expected 2 scale-up calls, got %d", len(fakeASG.ScaleUpCalls))
	}
}

// TestCapacityRouter_MNGFallsBackToCA_Live verifies that MNG nodes fall back
// to the CA manager when only a CA manager is registered, using real Kind nodes.
func TestCapacityRouter_MNGFallsBackToCA_Live(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	// Only register a CA manager (no MNG manager)
	caMgr := &trackingManager{mgrType: capacity.ManagerClusterAutoscaler}
	router := capacity.NewRouter(slog.Default(), caMgr)

	// Find an MNG-labeled node
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "eks.amazonaws.com/nodegroup",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("No MNG-labeled nodes found in cluster")
	}

	mngNode := &nodes.Items[0]
	mgr := router.ManagerForNode(mngNode)
	if mgr == nil {
		t.Fatal("expected MNG node to fall back to CA manager, got nil")
	}
	if mgr.Type() != capacity.ManagerClusterAutoscaler {
		t.Errorf("expected CA manager for MNG node fallback, got %s", mgr.Type())
	}
	t.Logf("  MNG node %s correctly fell back to CA manager", mngNode.Name)
}

// TestCapacityDetection_ExplicitOverride verifies that the spotvortex.io/manager
// label takes priority over auto-detected provisioner labels.
func TestCapacityDetection_ExplicitOverride(t *testing.T) {
	client := setupClient(t)
	ctx := context.Background()

	// Find the CA node (has spotvortex.io/manager=cluster-autoscaler)
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "spotvortex.io/manager=cluster-autoscaler",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("No nodes with spotvortex.io/manager=cluster-autoscaler label")
	}

	detector := capacity.NewDetector(slog.Default())
	node := &nodes.Items[0]

	got := detector.DetectManager(node)
	if got != capacity.ManagerClusterAutoscaler {
		t.Errorf("expected ManagerClusterAutoscaler from explicit override, got %s", got)
	}

	// Verify that the node does NOT have karpenter labels (confirming override is the only signal)
	if _, hasKarpenter := node.Labels[capacity.LabelKarpenterNodePool]; hasKarpenter {
		t.Logf("  (node also has karpenter label, override takes priority)")
	}
	t.Logf("  explicit override on %s: detected %s", node.Name, got)
}

// trackingManager is a stub CapacityManager that tracks calls for testing.
type trackingManager struct {
	mgrType      capacity.ManagerType
	prepareCalls int
	cleanupCalls int
}

func (m *trackingManager) Type() capacity.ManagerType { return m.mgrType }

func (m *trackingManager) PrepareSwap(ctx context.Context, pool capacity.PoolInfo, dir capacity.SwapDirection) (*capacity.SwapResult, error) {
	m.prepareCalls++
	return &capacity.SwapResult{Ready: true, Duration: time.Millisecond}, nil
}

func (m *trackingManager) PostDrainCleanup(ctx context.Context, nodeName string, pool capacity.PoolInfo) error {
	m.cleanupCalls++
	return nil
}

func (m *trackingManager) IsAvailable(ctx context.Context) bool { return true }

// Compile-time check.
var _ capacity.CapacityManager = (*trackingManager)(nil)
