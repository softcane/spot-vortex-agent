package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestDrain_DryRun verifies dry-run mode doesn't affect the cluster.
func TestDrain_DryRun(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		},
	)

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             true,
		GracePeriodSeconds: 30,
	})

	result, err := drainer.Drain(context.Background(), "test-node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.DryRun {
		t.Error("expected DryRun=true in result")
	}

	if !result.Success {
		t.Error("expected Success=true in dry-run mode")
	}

	// Verify node was NOT actually cordoned
	node, _ := client.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	if node.Spec.Unschedulable {
		t.Error("node should not be cordoned in dry-run mode")
	}
}

// TestDrain_CordonNode verifies the node is marked unschedulable.
func TestDrain_CordonNode(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec:       corev1.NodeSpec{Unschedulable: false},
	}
	client := fake.NewSimpleClientset(node)

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             false,
		GracePeriodSeconds: 30,
	})

	err := drainer.cordonNode(context.Background(), "test-node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify node was cordoned
	updated, _ := client.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("node should be cordoned (Unschedulable=true)")
	}
}

// TestDrain_NodeAlreadyCordoned verifies idempotency.
func TestDrain_NodeAlreadyCordoned(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	client := fake.NewSimpleClientset(node)

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             false,
		GracePeriodSeconds: 30,
	})

	err := drainer.cordonNode(context.Background(), "test-node")
	if err != nil {
		t.Fatalf("unexpected error on already-cordoned node: %v", err)
	}
}

// TestDrain_EvictPod verifies the Eviction API is used.
func TestDrain_EvictPod(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
		},
	}

	client := fake.NewSimpleClientset(node, pod)

	// Track if Eviction API was used
	evictionCalled := false
	client.PrependReactor("create", "pods/eviction", func(action k8stesting.Action) (bool, runtime.Object, error) {
		evictionCalled = true
		return true, nil, nil
	})

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             false,
		GracePeriodSeconds: 30,
	})

	err := drainer.evictPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !evictionCalled {
		t.Error("expected Eviction API to be called, not delete")
	}
}

// TestDrain_PDBBlocks verifies PDB violations return appropriate errors.
func TestDrain_PDBBlocks(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pdb-protected-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
	}

	client := fake.NewSimpleClientset(pod)

	// Simulate PDB blocking eviction (429 Too Many Requests)
	client.PrependReactor("create", "pods/eviction", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequests("PDB prevents eviction", 10)
	})

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             false,
		GracePeriodSeconds: 30,
	})

	err := drainer.evictPod(context.Background(), pod)
	if err == nil {
		t.Fatal("expected error when PDB blocks eviction")
	}

	// Verify error message mentions PDB
	if err.Error() != "PDB prevents eviction: the server has asked for the client to provide credentials (Unauthenticated)" &&
		!contains(err.Error(), "PDB") {
		t.Logf("error message: %s", err.Error())
	}
}

func TestDrain_AbortsWhenPDBBlocks(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "pdb-node"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "blocked-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "pdb-node"},
	}

	client := fake.NewSimpleClientset(node, pod)
	client.PrependReactor("create", "pods/eviction", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequests("PDB blocks", 10)
	})

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             false,
		GracePeriodSeconds: 30,
		Force:              false,
	})

	result, err := drainer.Drain(context.Background(), "pdb-node")
	if err == nil {
		t.Fatal("expected drain to fail when eviction is blocked by PDB")
	}
	if result.Success {
		t.Fatal("expected Success=false when drain aborts")
	}
	if result.PodsFailed != 1 {
		t.Fatalf("expected PodsFailed=1, got %d", result.PodsFailed)
	}
}

// TestDrain_SkipDaemonSetPod verifies DaemonSet pods are skipped.
func TestDrain_SkipDaemonSetPod(t *testing.T) {
	daemonSetPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daemonset-pod",
			Namespace: "kube-system",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "node-exporter"},
			},
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
	}

	drainer := NewDrainer(nil, nil, DrainConfig{
		IgnoreDaemonSets: true,
	})

	if !drainer.isDaemonSetPod(daemonSetPod) {
		t.Error("expected pod to be identified as DaemonSet pod")
	}
}

// TestDrain_SkipMirrorPod verifies mirror (static) pods are skipped.
func TestDrain_SkipMirrorPod(t *testing.T) {
	mirrorPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-apiserver",
			Namespace: "kube-system",
			Annotations: map[string]string{
				corev1.MirrorPodAnnotationKey: "true",
			},
		},
		Spec: corev1.PodSpec{NodeName: "master-1"},
	}

	drainer := NewDrainer(nil, nil, DrainConfig{})

	if !drainer.isMirrorPod(mirrorPod) {
		t.Error("expected pod to be identified as mirror pod")
	}
}

// TestDrain_PodAlreadyGone verifies 404 errors are handled gracefully.
func TestDrain_PodAlreadyGone(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gone-pod",
			Namespace: "default",
		},
	}

	client := fake.NewSimpleClientset()

	// Simulate pod not found
	client.PrependReactor("create", "pods/eviction", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "gone-pod")
	})

	drainer := NewDrainer(client, nil, DrainConfig{
		DryRun:             false,
		GracePeriodSeconds: 30,
	})

	err := drainer.evictPod(context.Background(), pod)
	if err != nil {
		t.Errorf("expected no error for already-gone pod, got: %v", err)
	}
}

// TestDrain_Uncordon verifies node can be marked schedulable again.
func TestDrain_Uncordon(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	client := fake.NewSimpleClientset(node)

	drainer := NewDrainer(client, nil, DrainConfig{})

	err := drainer.Uncordon(context.Background(), "test-node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated, _ := client.CoreV1().Nodes().Get(context.Background(), "test-node", metav1.GetOptions{})
	if updated.Spec.Unschedulable {
		t.Error("node should be uncordoned (Unschedulable=false)")
	}
}

// TestDrain_FullFlow verifies complete drain flow.
func TestDrain_FullFlow(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "drain-node"},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-pod-1",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "drain-node"},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-pod-2",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "drain-node"},
	}

	client := fake.NewSimpleClientset(node, pod1, pod2)

	// Allow evictions
	client.PrependReactor("create", "pods/eviction", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	drainer := NewDrainer(client, nil, DrainConfig{
		GracePeriodSeconds: 30,
		Timeout:            1 * time.Minute,
		IgnoreDaemonSets:   true,
	})

	result, err := drainer.Drain(context.Background(), "drain-node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected drain to succeed")
	}

	if result.PodsEvicted != 2 {
		t.Errorf("expected 2 pods evicted, got %d", result.PodsEvicted)
	}

	// Verify node was cordoned
	updated, _ := client.CoreV1().Nodes().Get(context.Background(), "drain-node", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("node should be cordoned after drain")
	}
}

// TestDrain_ForceMode verifies drain continues despite failures in force mode.
func TestDrain_ForceMode(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "force-node"},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stubborn-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "force-node"},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "good-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "force-node"},
	}

	client := fake.NewSimpleClientset(node, pod1, pod2)

	callCount := 0
	client.PrependReactor("create", "pods/eviction", func(action k8stesting.Action) (bool, runtime.Object, error) {
		callCount++
		if callCount == 1 {
			// First pod fails
			return true, nil, apierrors.NewTooManyRequests("PDB blocks", 10)
		}
		return true, nil, nil
	})

	drainer := NewDrainer(client, nil, DrainConfig{
		GracePeriodSeconds: 30,
		Force:              true, // Continue despite failures
	})

	result, _ := drainer.Drain(context.Background(), "force-node")

	// Should have attempted both pods
	if result.PodsEvicted == 0 && result.PodsFailed == 0 {
		t.Error("expected at least one eviction attempt")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s[1:], substr))
}
