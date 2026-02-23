package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestKubernetesReliabilityTelemetryCollector_CollectsNodeTransitionsAndPodRestarts(t *testing.T) {
	ctx := context.Background()
	client := k8sfake.NewSimpleClientset(
		testNode("node-a", "uid-node-a", true),
		testNode("node-b", "uid-node-b", true),
		testPodRunning("default", "app-1", "uid-pod-1", 0),
	)

	collector := NewKubernetesReliabilityTelemetryCollector(client, slog.Default())
	if collector == nil {
		t.Fatal("expected collector")
	}

	first, err := collector.CollectReliabilityTelemetry(ctx)
	if err != nil {
		t.Fatalf("first collect failed: %v", err)
	}
	if first.NodeNotReadyTransitions != 0 || first.NodeTerminations != 0 || first.PodRestarts != 0 {
		t.Fatalf("expected baseline snapshot to be zeroed, got %+v", first)
	}

	nodeA, err := client.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node-a: %v", err)
	}
	setNodeReady(nodeA, false)
	if _, err := client.CoreV1().Nodes().Update(ctx, nodeA, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update node-a: %v", err)
	}
	if err := client.CoreV1().Nodes().Delete(ctx, "node-b", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete node-b: %v", err)
	}

	pod, err := client.CoreV1().Pods("default").Get(ctx, "app-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	pod.Status.ContainerStatuses[0].RestartCount = 2
	if _, err := client.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	second, err := collector.CollectReliabilityTelemetry(ctx)
	if err != nil {
		t.Fatalf("second collect failed: %v", err)
	}
	if second.NodeNotReadyTransitions != 1 {
		t.Fatalf("NodeNotReadyTransitions=%d, want 1", second.NodeNotReadyTransitions)
	}
	if second.NodeTerminations != 1 {
		t.Fatalf("NodeTerminations=%d, want 1", second.NodeTerminations)
	}
	if second.PodRestarts != 2 {
		t.Fatalf("PodRestarts=%d, want 2", second.PodRestarts)
	}
}

func TestKubernetesReliabilityTelemetryCollector_RecordsPendingAndRecoveryDurations(t *testing.T) {
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	now := base.Add(5 * time.Second)

	client := k8sfake.NewSimpleClientset(testPodPending("default", "api-1", "uid-pod-pending", base))
	collector := NewKubernetesReliabilityTelemetryCollector(client, slog.Default())
	if collector == nil {
		t.Fatal("expected collector")
	}
	collector.now = func() time.Time { return now }

	first, err := collector.CollectReliabilityTelemetry(ctx)
	if err != nil {
		t.Fatalf("first collect failed: %v", err)
	}
	if len(first.PodPendingDurationsSeconds) != 0 || len(first.RecoveryDurationsSeconds) != 0 {
		t.Fatalf("expected no duration samples on first pending observation, got %+v", first)
	}

	now = base.Add(30 * time.Second)
	pod, err := client.CoreV1().Pods("default").Get(ctx, "api-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pending pod: %v", err)
	}
	start := metav1.NewTime(base.Add(20 * time.Second))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.StartTime = &start
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 0}}
	if _, err := client.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod to running: %v", err)
	}

	second, err := collector.CollectReliabilityTelemetry(ctx)
	if err != nil {
		t.Fatalf("second collect failed: %v", err)
	}
	if got := len(second.PodPendingDurationsSeconds); got != 1 {
		t.Fatalf("pending duration sample count=%d, want 1", got)
	}
	if got := len(second.RecoveryDurationsSeconds); got != 1 {
		t.Fatalf("recovery duration sample count=%d, want 1", got)
	}
	if second.PodPendingDurationsSeconds[0] != 20 {
		t.Fatalf("pending duration=%v, want 20", second.PodPendingDurationsSeconds[0])
	}
	if second.RecoveryDurationsSeconds[0] != 20 {
		t.Fatalf("recovery duration=%v, want 20", second.RecoveryDurationsSeconds[0])
	}
}

func testNode(name, uid string, ready bool) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(uid),
		},
	}
	setNodeReady(n, ready)
	return n
}

func setNodeReady(node *corev1.Node, ready bool) {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:   corev1.NodeReady,
			Status: status,
		},
	}
}

func testPodRunning(namespace, name, uid string, restarts int32) *corev1.Pod {
	now := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			UID:               types.UID(uid),
			CreationTimestamp: now,
		},
		Status: corev1.PodStatus{
			Phase:     corev1.PodRunning,
			StartTime: &now,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: restarts},
			},
		},
	}
}

func testPodPending(namespace, name, uid string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
}
