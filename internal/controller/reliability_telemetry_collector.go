package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	svmetrics "github.com/softcane/spot-vortex-agent/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KubernetesReliabilityTelemetryCollector collects small, truthful reliability
// signals from the Kubernetes API using periodic polling.
//
// Signals currently implemented:
// - node Ready -> NotReady transitions
// - observed node deletions between polling cycles
// - pod container restart count deltas
// - pod pending -> running duration samples (recorded as both pending/recovery)
//
// AWS interruption/rebalance signals are intentionally not faked here and stay zero.
type KubernetesReliabilityTelemetryCollector struct {
	k8s    kubernetes.Interface
	logger *slog.Logger

	mu sync.Mutex

	now func() time.Time

	lastNodeReady          map[string]bool
	lastSeenNodes          map[string]struct{}
	lastContainerRestarts  map[string]int32
	lastPodLifecycleStatus map[string]podLifecycleStatus
}

type podLifecycleStatus struct {
	phase        corev1.PodPhase
	pendingSince time.Time
}

func NewKubernetesReliabilityTelemetryCollector(k8s kubernetes.Interface, logger *slog.Logger) *KubernetesReliabilityTelemetryCollector {
	if k8s == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &KubernetesReliabilityTelemetryCollector{
		k8s:                    k8s,
		logger:                 logger,
		now:                    time.Now,
		lastNodeReady:          map[string]bool{},
		lastSeenNodes:          map[string]struct{}{},
		lastContainerRestarts:  map[string]int32{},
		lastPodLifecycleStatus: map[string]podLifecycleStatus{},
	}
}

func (c *KubernetesReliabilityTelemetryCollector) CollectReliabilityTelemetry(ctx context.Context) (svmetrics.ReliabilityTelemetrySnapshot, error) {
	if c == nil || c.k8s == nil {
		return svmetrics.ReliabilityTelemetrySnapshot{}, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now
	if now == nil {
		now = time.Now
	}

	snapshot := svmetrics.ReliabilityTelemetrySnapshot{}
	c.collectNodeSignals(ctx, &snapshot)
	c.collectPodSignals(ctx, now(), &snapshot)
	return snapshot, nil
}

func (c *KubernetesReliabilityTelemetryCollector) collectNodeSignals(ctx context.Context, snapshot *svmetrics.ReliabilityTelemetrySnapshot) {
	nodes, err := c.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Warn("reliability telemetry: failed to list nodes", "error", err)
		return
	}

	currentSeen := make(map[string]struct{}, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		nodeKey := nodeIdentity(node)
		currentSeen[nodeKey] = struct{}{}

		ready := isNodeReady(node)
		if prevReady, ok := c.lastNodeReady[nodeKey]; ok && prevReady && !ready {
			snapshot.NodeNotReadyTransitions++
		}
		c.lastNodeReady[nodeKey] = ready
	}

	// Count nodes that disappeared since the last poll as observed deletions.
	for nodeKey := range c.lastSeenNodes {
		if _, ok := currentSeen[nodeKey]; ok {
			continue
		}
		snapshot.NodeTerminations++
		delete(c.lastNodeReady, nodeKey)
	}
	c.lastSeenNodes = currentSeen
}

func (c *KubernetesReliabilityTelemetryCollector) collectPodSignals(ctx context.Context, now time.Time, snapshot *svmetrics.ReliabilityTelemetrySnapshot) {
	pods, err := c.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Warn("reliability telemetry: failed to list pods", "error", err)
		return
	}

	currentPods := make(map[string]struct{}, len(pods.Items))
	currentContainers := map[string]struct{}{}

	for i := range pods.Items {
		pod := &pods.Items[i]
		podKey := podIdentity(pod)
		currentPods[podKey] = struct{}{}

		prev := c.lastPodLifecycleStatus[podKey]
		next := podLifecycleStatus{phase: pod.Status.Phase}

		switch pod.Status.Phase {
		case corev1.PodPending:
			if prev.phase == corev1.PodPending && !prev.pendingSince.IsZero() {
				next.pendingSince = prev.pendingSince
			} else {
				next.pendingSince = pendingStartTime(pod, now)
			}
		default:
			if prev.phase == corev1.PodPending && !prev.pendingSince.IsZero() {
				d := pendingDurationSeconds(prev.pendingSince, pod, now)
				snapshot.PodPendingDurationsSeconds = append(snapshot.PodPendingDurationsSeconds, d)
				if pod.Status.Phase == corev1.PodRunning {
					snapshot.RecoveryDurationsSeconds = append(snapshot.RecoveryDurationsSeconds, d)
				}
			}
		}
		c.lastPodLifecycleStatus[podKey] = next

		for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			key := podKey + "/" + cs.Name
			currentContainers[key] = struct{}{}
			if prevRestarts, ok := c.lastContainerRestarts[key]; ok {
				if cs.RestartCount > prevRestarts {
					snapshot.PodRestarts += uint64(cs.RestartCount - prevRestarts)
				}
			}
			c.lastContainerRestarts[key] = cs.RestartCount
		}
	}

	for podKey := range c.lastPodLifecycleStatus {
		if _, ok := currentPods[podKey]; !ok {
			delete(c.lastPodLifecycleStatus, podKey)
		}
	}
	for containerKey := range c.lastContainerRestarts {
		if _, ok := currentContainers[containerKey]; !ok {
			delete(c.lastContainerRestarts, containerKey)
		}
	}
}

func isNodeReady(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeIdentity(node *corev1.Node) string {
	if node == nil {
		return ""
	}
	if node.UID != "" {
		return string(node.UID)
	}
	return node.Name
}

func podIdentity(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	if pod.UID != "" {
		return string(pod.UID)
	}
	return pod.Namespace + "/" + pod.Name
}

func pendingStartTime(pod *corev1.Pod, now time.Time) time.Time {
	if pod != nil && !pod.CreationTimestamp.IsZero() {
		start := pod.CreationTimestamp.Time
		if start.After(now) {
			return now
		}
		return start
	}
	return now
}

func pendingDurationSeconds(start time.Time, pod *corev1.Pod, now time.Time) float64 {
	end := now
	if pod != nil && pod.Status.Phase == corev1.PodRunning && pod.Status.StartTime != nil && !pod.Status.StartTime.IsZero() {
		end = pod.Status.StartTime.Time
	}
	if start.IsZero() {
		start = end
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Seconds()
}
