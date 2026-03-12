package capacity

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestFakeASGClient_TwinDiscovery(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("web-backend", 3, 1)

	ctx := context.Background()
	spot, od, err := client.DiscoverTwinASGs(ctx, "web-backend")
	if err != nil {
		t.Fatalf("DiscoverTwinASGs: %v", err)
	}

	if spot.Pool != "web-backend" || spot.CapacityType != "spot" {
		t.Errorf("spot ASG: pool=%q type=%q", spot.Pool, spot.CapacityType)
	}
	if od.Pool != "web-backend" || od.CapacityType != "on-demand" {
		t.Errorf("OD ASG: pool=%q type=%q", od.Pool, od.CapacityType)
	}
	if spot.DesiredCapacity != 3 {
		t.Errorf("spot desired=%d, want 3", spot.DesiredCapacity)
	}
	if od.DesiredCapacity != 1 {
		t.Errorf("OD desired=%d, want 1", od.DesiredCapacity)
	}
}

func TestFakeASGClient_DiscoveryNotFound(t *testing.T) {
	client := NewFakeASGClient()
	ctx := context.Background()

	_, _, err := client.DiscoverTwinASGs(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent pool")
	}
}

func TestFakeASGClient_ScaleUp(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("web", 2, 0)

	ctx := context.Background()
	err := client.SetDesiredCapacity(ctx, "web-od-asg", 1)
	if err != nil {
		t.Fatalf("SetDesiredCapacity: %v", err)
	}

	asg := client.GetASG("web-od-asg")
	if asg.DesiredCapacity != 1 {
		t.Errorf("desired=%d, want 1", asg.DesiredCapacity)
	}
	if asg.CurrentCount != 1 {
		t.Errorf("current=%d, want 1 (fake scales instantly)", asg.CurrentCount)
	}

	if len(client.ScaleUpCalls) != 1 {
		t.Errorf("scale calls=%d, want 1", len(client.ScaleUpCalls))
	}
}

func TestFakeASGClient_ScaleExceedsMax(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("web", 2, 0) // max = 0+5=5 for OD

	ctx := context.Background()
	err := client.SetDesiredCapacity(ctx, "web-od-asg", 10) // exceeds max 5
	if err == nil {
		t.Error("expected error when exceeding max")
	}
}

func TestFakeASGClient_TerminateInstance(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("web", 3, 1)

	ctx := context.Background()
	err := client.TerminateInstance(ctx, "web-spot-asg", "i-12345", true)
	if err != nil {
		t.Fatalf("TerminateInstance: %v", err)
	}

	asg := client.GetASG("web-spot-asg")
	if asg.DesiredCapacity != 2 {
		t.Errorf("desired=%d, want 2 (decremented)", asg.DesiredCapacity)
	}
	if asg.CurrentCount != 2 {
		t.Errorf("current=%d, want 2", asg.CurrentCount)
	}

	if len(client.TerminateCalls) != 1 {
		t.Errorf("terminate calls=%d, want 1", len(client.TerminateCalls))
	}
}

func TestASGManager_PrepareSwap_ToOnDemand(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("api-pool", 3, 1)

	mgr := NewASGManager(ASGManagerConfig{
		ASGClient:        client,
		Logger:           slog.Default(),
		ManagerType:      ManagerClusterAutoscaler,
		NodeReadyTimeout: 2 * time.Second,
		PollInterval:     100 * time.Millisecond,
	})

	// Type check
	if mgr.Type() != ManagerClusterAutoscaler {
		t.Errorf("Type() = %q, want %q", mgr.Type(), ManagerClusterAutoscaler)
	}

	ctx := context.Background()
	result, err := mgr.PrepareSwap(ctx, PoolInfo{Name: "api-pool", Zone: "us-east-1a"}, SwapToOnDemand)
	if err != nil {
		t.Fatalf("PrepareSwap: %v", err)
	}
	if !result.Ready {
		t.Error("expected Ready=true")
	}

	// Verify OD ASG was scaled up
	odASG := client.GetASG("api-pool-od-asg")
	if odASG.DesiredCapacity != 2 { // was 1, scaled to 2
		t.Errorf("OD desired=%d, want 2", odASG.DesiredCapacity)
	}

	// Verify spot ASG was NOT touched
	spotASG := client.GetASG("api-pool-spot-asg")
	if spotASG.DesiredCapacity != 3 { // unchanged
		t.Errorf("spot desired=%d, want 3 (unchanged)", spotASG.DesiredCapacity)
	}
}

func TestASGManager_PrepareSwap_ToSpot(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("batch-pool", 1, 3)

	mgr := NewASGManager(ASGManagerConfig{
		ASGClient:        client,
		Logger:           slog.Default(),
		ManagerType:      ManagerManagedNodegroup,
		NodeReadyTimeout: 2 * time.Second,
		PollInterval:     100 * time.Millisecond,
	})

	if mgr.Type() != ManagerManagedNodegroup {
		t.Errorf("Type() = %q, want %q", mgr.Type(), ManagerManagedNodegroup)
	}

	ctx := context.Background()
	result, err := mgr.PrepareSwap(ctx, PoolInfo{Name: "batch-pool"}, SwapToSpot)
	if err != nil {
		t.Fatalf("PrepareSwap: %v", err)
	}
	if !result.Ready {
		t.Error("expected Ready=true")
	}

	// Verify spot ASG was scaled up
	spotASG := client.GetASG("batch-pool-spot-asg")
	if spotASG.DesiredCapacity != 2 { // was 1, scaled to 2
		t.Errorf("spot desired=%d, want 2", spotASG.DesiredCapacity)
	}
}

func TestASGManager_PrepareSwap_NilClient(t *testing.T) {
	mgr := NewASGManager(ASGManagerConfig{
		ASGClient: nil,
		Logger:    slog.Default(),
	})

	ctx := context.Background()
	_, err := mgr.PrepareSwap(ctx, PoolInfo{Name: "test"}, SwapToOnDemand)
	if err == nil {
		t.Error("expected error with nil ASG client")
	}
}

func TestASGManager_PrepareSwap_PoolNotFound(t *testing.T) {
	client := NewFakeASGClient()
	// Don't add any twin pairs

	mgr := NewASGManager(ASGManagerConfig{
		ASGClient:        client,
		Logger:           slog.Default(),
		NodeReadyTimeout: 1 * time.Second,
		PollInterval:     100 * time.Millisecond,
	})

	ctx := context.Background()
	_, err := mgr.PrepareSwap(ctx, PoolInfo{Name: "missing-pool"}, SwapToOnDemand)
	if err == nil {
		t.Error("expected error for missing twin ASG pair")
	}
}

func TestASGManager_PostDrainCleanup(t *testing.T) {
	client := NewFakeASGClient()
	mgr := NewASGManager(ASGManagerConfig{
		ASGClient: client,
		Logger:    slog.Default(),
	})

	ctx := context.Background()
	err := mgr.PostDrainCleanup(ctx, "test-node", PoolInfo{Name: "test-pool"})
	if err != nil {
		t.Errorf("PostDrainCleanup: %v", err)
	}
}

func TestASGManager_PostDrainCleanup_TerminatesSourceASG(t *testing.T) {
	client := NewFakeASGClient()
	client.AddTwinPair("test-pool", 3, 1)

	k8sClient := k8sfake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				"spotvortex.io/pool":          "test-pool",
				LabelSpotVortexCapacity:       "spot",
				"topology.kubernetes.io/zone": "us-east-1a",
			},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-east-1a/i-1234567890abcdef0",
		},
	})

	mgr := NewASGManager(ASGManagerConfig{
		ASGClient: client,
		K8sClient: k8sClient,
		Logger:    slog.Default(),
	})

	err := mgr.PostDrainCleanup(context.Background(), "test-node", PoolInfo{Name: "test-pool"})
	if err != nil {
		t.Fatalf("PostDrainCleanup: %v", err)
	}

	if len(client.TerminateCalls) != 1 {
		t.Fatalf("terminate calls=%d, want 1", len(client.TerminateCalls))
	}
	call := client.TerminateCalls[0]
	if call.ASGID != "test-pool-spot-asg" {
		t.Fatalf("cleanup ASG=%q, want %q", call.ASGID, "test-pool-spot-asg")
	}
	if call.InstanceID != "i-1234567890abcdef0" {
		t.Fatalf("cleanup instance=%q, want %q", call.InstanceID, "i-1234567890abcdef0")
	}
	if !call.Decrement {
		t.Fatal("expected cleanup to decrement desired capacity")
	}
}

func TestInstanceIDFromProviderID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       string
	}{
		{name: "aws triple slash", providerID: "aws:///us-east-1a/i-123", want: "i-123"},
		{name: "aws double slash", providerID: "aws://us-east-1a/i-456", want: "i-456"},
		{name: "bare id", providerID: "i-789", want: "i-789"},
		{name: "empty", providerID: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := instanceIDFromProviderID(tc.providerID); got != tc.want {
				t.Fatalf("instanceIDFromProviderID(%q) = %q, want %q", tc.providerID, got, tc.want)
			}
		})
	}
}

func TestASGManager_WaitForNewNode_UsesNormalizedCapacityLabels(t *testing.T) {
	k8sClient := k8sfake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "existing-node",
				Labels: map[string]string{
					"spotvortex.io/pool": "api",
				},
			},
		},
	)
	mgr := NewASGManager(ASGManagerConfig{
		K8sClient:        k8sClient,
		Logger:           slog.Default(),
		NodeReadyTimeout: 500 * time.Millisecond,
		PollInterval:     10 * time.Millisecond,
	})

	go func() {
		time.Sleep(25 * time.Millisecond)
		_, _ = k8sClient.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "replacement-node",
				Labels: map[string]string{
					"spotvortex.io/pool":    "api",
					LabelEKSCapacityType:    "ON_DEMAND",
					LabelSpotVortexCapacity: "on-demand",
				},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				}},
			},
		}, metav1.CreateOptions{})
	}()

	nodeName, err := mgr.waitForNewNode(context.Background(), PoolInfo{Name: "api"}, SwapToOnDemand)
	if err != nil {
		t.Fatalf("waitForNewNode: %v", err)
	}
	if nodeName != "replacement-node" {
		t.Fatalf("replacement node = %q, want %q", nodeName, "replacement-node")
	}
}

func TestASGManager_IsAvailable(t *testing.T) {
	mgr := NewASGManager(ASGManagerConfig{
		ASGClient: NewFakeASGClient(),
		Logger:    slog.Default(),
	})
	if !mgr.IsAvailable(context.Background()) {
		t.Error("expected IsAvailable=true with fake client")
	}

	mgrNoClient := NewASGManager(ASGManagerConfig{Logger: slog.Default()})
	if mgrNoClient.IsAvailable(context.Background()) {
		t.Error("expected IsAvailable=false with nil client")
	}
}
