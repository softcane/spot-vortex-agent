// Package controller implements the SpotVortex drain logic.
// Uses Kubernetes Eviction API to respect PodDisruptionBudgets.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DrainConfig configures the drain operation.
type DrainConfig struct {
	// GracePeriodSeconds is the grace period for pod termination.
	GracePeriodSeconds int64

	// Timeout is the maximum time to wait for drain completion.
	Timeout time.Duration

	// DryRun enables dry-run mode (no actual evictions).
	DryRun bool

	// IgnoreDaemonSets skips DaemonSet pods during drain.
	IgnoreDaemonSets bool

	// DeleteEmptyDirData allows deletion of pods with emptyDir volumes.
	DeleteEmptyDirData bool

	// Force allows drain even if some pods cannot be evicted.
	Force bool
}

// DrainResult represents the outcome of a drain operation.
type DrainResult struct {
	NodeName    string
	Success     bool
	DryRun      bool
	PodsEvicted int
	PodsSkipped int
	PodsFailed  int
	Duration    time.Duration
	FailedPods  []string
	Error       error
}

// Drainer handles node drain operations using Eviction API.
type Drainer struct {
	client kubernetes.Interface
	logger *slog.Logger
	config DrainConfig
}

// NewDrainer creates a new Drainer instance.
func NewDrainer(client kubernetes.Interface, logger *slog.Logger, config DrainConfig) *Drainer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Drainer{
		client: client,
		logger: logger,
		config: config,
	}
}

// Drain cordons and evicts all pods from a node.
// Uses Eviction API to respect PodDisruptionBudgets (PDBs).
//
// Prime Directive: This is a critical operation. Ensure PDBs are respected
// and fallback to On-Demand if eviction fails.
func (d *Drainer) Drain(ctx context.Context, nodeName string) (*DrainResult, error) {
	start := time.Now()
	result := &DrainResult{
		NodeName: nodeName,
		DryRun:   d.config.DryRun,
	}

	d.logger.Info("starting node drain",
		"node_id", nodeName,
		"dry_run", d.config.DryRun,
		"grace_period_seconds", d.config.GracePeriodSeconds,
	)

	// Step 1: Cordon the node (mark unschedulable)
	if err := d.cordonNode(ctx, nodeName); err != nil {
		result.Error = fmt.Errorf("failed to cordon node: %w", err)
		return result, result.Error
	}

	// Step 2: Get all pods on the node
	pods, err := d.getPodsOnNode(ctx, nodeName)
	if err != nil {
		result.Error = fmt.Errorf("failed to list pods: %w", err)
		return result, result.Error
	}

	d.logger.Info("found pods to evict",
		"node_id", nodeName,
		"pod_count", len(pods),
	)

	// Step 3: Evict each pod using Eviction API
	for _, pod := range pods {
		// Skip DaemonSet pods if configured
		if d.config.IgnoreDaemonSets && d.isDaemonSetPod(&pod) {
			result.PodsSkipped++
			continue
		}

		// Skip mirror pods (static pods)
		if d.isMirrorPod(&pod) {
			result.PodsSkipped++
			continue
		}

		if err := d.evictPod(ctx, &pod); err != nil {
			d.logger.Warn("failed to evict pod",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"error", err,
			)
			result.PodsFailed++
			result.FailedPods = append(result.FailedPods, pod.Namespace+"/"+pod.Name)

			// If not forcing, abort on first failure
			if !d.config.Force {
				result.Error = fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
				result.Duration = time.Since(start)
				return result, result.Error
			}
		} else {
			result.PodsEvicted++
		}
	}

	result.Success = result.PodsFailed == 0
	result.Duration = time.Since(start)

	d.logger.Info("drain complete",
		"node_id", nodeName,
		"success", result.Success,
		"pods_evicted", result.PodsEvicted,
		"pods_skipped", result.PodsSkipped,
		"pods_failed", result.PodsFailed,
		"duration", result.Duration,
	)

	return result, nil
}

// cordonNode marks the node as unschedulable.
func (d *Drainer) cordonNode(ctx context.Context, nodeName string) error {
	d.logger.Debug("cordoning node", "node_id", nodeName)

	if d.config.DryRun {
		d.logger.Info("dry-run: would cordon node", "node_id", nodeName)
		return nil
	}

	node, err := d.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if node.Spec.Unschedulable {
		d.logger.Debug("node already cordoned", "node_id", nodeName)
		return nil
	}

	node.Spec.Unschedulable = true
	_, err = d.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

// getPodsOnNode returns all pods running on a node.
func (d *Drainer) getPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	podList, err := d.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// evictPod evicts a single pod using the Eviction API.
// This respects PodDisruptionBudgets (PDBs).
func (d *Drainer) evictPod(ctx context.Context, pod *corev1.Pod) error {
	if d.config.DryRun {
		d.logger.Info("dry-run: would evict pod",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"node_id", pod.Spec.NodeName,
		)
		return nil
	}

	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: &d.config.GracePeriodSeconds,
		},
	}

	err := d.client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, eviction)
	if err != nil {
		// Check if pod is already gone
		if apierrors.IsNotFound(err) {
			return nil
		}
		// PDB violation - important to surface this
		if apierrors.IsTooManyRequests(err) {
			return fmt.Errorf("PDB prevents eviction: %w", err)
		}
		return err
	}

	d.logger.Debug("evicted pod",
		"pod", pod.Name,
		"namespace", pod.Namespace,
	)

	return nil
}

// isDaemonSetPod checks if a pod is managed by a DaemonSet.
func (d *Drainer) isDaemonSetPod(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isMirrorPod checks if a pod is a static/mirror pod.
func (d *Drainer) isMirrorPod(pod *corev1.Pod) bool {
	_, exists := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return exists
}

// Uncordon marks a node as schedulable again.
func (d *Drainer) Uncordon(ctx context.Context, nodeName string) error {
	d.logger.Info("uncordoning node", "node_id", nodeName)

	if d.config.DryRun {
		d.logger.Info("dry-run: would uncordon node", "node_id", nodeName)
		return nil
	}

	node, err := d.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if !node.Spec.Unschedulable {
		return nil
	}

	node.Spec.Unschedulable = false
	_, err = d.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}
