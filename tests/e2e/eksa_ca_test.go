package e2e

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/softcane/spot-vortex-agent/internal/capacity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const e2eSuiteEnv = "SPOTVORTEX_E2E_SUITE"

func requireEKSAnywhereCASuite(t *testing.T) {
	t.Helper()
	if os.Getenv(e2eSuiteEnv) != "eksa-ca" {
		t.Skipf("set %s=eksa-ca to run this suite", e2eSuiteEnv)
	}
}

func TestEKSAnywhereCA_ClusterAutoscalerDeploymentReady(t *testing.T) {
	requireEKSAnywhereCASuite(t)

	client := setupClient(t)
	ctx := context.Background()

	deployments, err := client.AppsV1().Deployments("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list kube-system deployments: %v", err)
	}

	var found bool
	for _, d := range deployments.Items {
		if !strings.Contains(d.Name, "cluster-autoscaler") {
			continue
		}
		found = true
		desiredReplicas := int32(0)
		if d.Spec.Replicas != nil {
			desiredReplicas = *d.Spec.Replicas
		}
		t.Logf("found autoscaler deployment %q (ready=%d available=%d desired=%d)",
			d.Name,
			d.Status.ReadyReplicas,
			d.Status.AvailableReplicas,
			desiredReplicas,
		)
		if d.Status.AvailableReplicas < 1 {
			t.Fatalf("cluster-autoscaler deployment %q has no available replicas", d.Name)
		}
	}

	if !found {
		t.Fatal("expected at least one cluster-autoscaler deployment in kube-system")
	}
}

func TestEKSAnywhereCA_DetectsClusterAutoscalerNodes(t *testing.T) {
	requireEKSAnywhereCASuite(t)

	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list nodes: %v", err)
	}

	detector := capacity.NewDetector(slog.Default())
	caNodeCount := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			continue
		}
		if node.Labels[capacity.LabelManagerOverride] != string(capacity.ManagerClusterAutoscaler) {
			continue
		}

		mgr := detector.DetectManager(node)
		if mgr != capacity.ManagerClusterAutoscaler {
			t.Fatalf("node %q detected manager=%s, want %s", node.Name, mgr, capacity.ManagerClusterAutoscaler)
		}
		caNodeCount++
	}

	if caNodeCount == 0 {
		t.Fatal("expected at least one worker node labeled for cluster-autoscaler routing")
	}
}

func TestEKSAnywhereCA_RouterUsesClusterAutoscalerManager(t *testing.T) {
	requireEKSAnywhereCASuite(t)

	client := setupClient(t)
	ctx := context.Background()

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: capacity.LabelManagerOverride + "=" + string(capacity.ManagerClusterAutoscaler),
	})
	if err != nil {
		t.Fatalf("failed to list cluster-autoscaler labeled nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("expected cluster-autoscaler labeled nodes")
	}

	caMgr := &trackingManager{mgrType: capacity.ManagerClusterAutoscaler}
	router := capacity.NewRouter(slog.Default(), caMgr)

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			continue
		}

		mgr := router.ManagerForNode(node)
		if mgr == nil {
			t.Fatalf("router returned nil for node %q", node.Name)
		}
		if mgr.Type() != capacity.ManagerClusterAutoscaler {
			t.Fatalf("router manager type=%s for node %q, want %s", mgr.Type(), node.Name, capacity.ManagerClusterAutoscaler)
		}
	}
}
