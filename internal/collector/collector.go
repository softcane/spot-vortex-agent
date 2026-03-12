// Package collector provides local metrics collection for SpotVortex.
//
// Collects pod startup latencies and PDB constraints locally
// without exporting to SaaS (Zero-PII guarantee).
//
// Based on: architecture.md (Prometheus/eBPF local source of truth)
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/softcane/spot-vortex-agent/internal/capacity"
	"github.com/softcane/spot-vortex-agent/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// WorkloadFeatures represents aggregated features for a node pool.
// Uses weighted averages for inference (reflects majority behavior) while
// keeping max values for guardrails (safety-critical decisions).
type WorkloadFeatures struct {
	PodStartupTime     float64 // Weighted P95 (seconds)
	OutagePenaltyHours float64 // Weighted average for inference
	PriorityScore      float64 // Weighted average 0-1 scale for inference
	ClusterUtilization float64 // Pool-level utilization
	PoolSafety         config.PoolSafetyVector

	// Max values for guardrails - a single critical pod triggers safety checks
	MaxOutagePenalty float64 // MAX across all pods (for guardrails)
	MaxPriorityScore float64 // MAX across all pods (for guardrails)
	HasCriticalPod   bool    // True if any P0/system-critical pod exists
}

// LocalMetrics holds cluster metrics collected locally
type LocalMetrics struct {
	// Features by PoolID (InstanceType:Zone)
	PoolFeatures map[string]WorkloadFeatures

	// Raw latencies for debugging
	PodStartupLatency map[string]float64

	// Last update time
	LastUpdated time.Time
}

// UtilizationProvider defines the interface for fetching cluster/pool utilization.
// This abstraction allows for different implementations (Prometheus, metrics-server, mock).
type UtilizationProvider interface {
	GetPoolUtilization(ctx context.Context) (map[string]float64, error)
}

// Collector gathers local cluster metrics for RL state
type Collector struct {
	client   kubernetes.Interface
	logger   *slog.Logger
	utilProv UtilizationProvider // Optional: for cluster utilization data

	mu      sync.RWMutex
	metrics LocalMetrics
}

// NewCollector creates a new local metrics collector
func NewCollector(client kubernetes.Interface, logger *slog.Logger) *Collector {
	return &Collector{
		client: client,
		logger: logger,
		metrics: LocalMetrics{
			PoolFeatures:      make(map[string]WorkloadFeatures),
			PodStartupLatency: make(map[string]float64),
		},
	}
}

// SetUtilizationProvider sets the utilization provider for cluster/pool utilization data.
// This should be called before Collect() to enable utilization-aware decisions.
func (c *Collector) SetUtilizationProvider(prov UtilizationProvider) {
	c.utilProv = prov
}

// Collect gathers current cluster metrics
func (c *Collector) Collect(ctx context.Context) (*LocalMetrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Debug("collecting local metrics")

	// 1. List Nodes to build Pool mapping
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error("failed to list nodes", "error", err)
		return nil, err
	}

	nodeToPools := make(map[string][]string)
	nodeIsSpot := make(map[string]bool)
	groupZones := make(map[string]map[string]struct{})
	poolStats := make(map[string]*poolAccumulator)
	for _, node := range nodes.Items {
		simplePoolID := GetNodePoolID(&node)
		extendedPoolID := GetExtendedPoolID(&node)
		poolKeys := []string{simplePoolID}
		if extendedPoolID != simplePoolID {
			poolKeys = append(poolKeys, extendedPoolID)
		}
		nodeToPools[node.Name] = poolKeys
		nodeIsSpot[node.Name] = capacity.IsSpotNode(&node)

		zone := node.Labels["topology.kubernetes.io/zone"]
		if zone == "" {
			zone = "unknown"
		}
		instanceType := node.Labels["node.kubernetes.io/instance-type"]
		if instanceType == "" {
			instanceType = "unknown"
		}
		workloadPool := node.Labels[WorkloadPoolLabel]

		simpleGroupKey := "instance:" + instanceType
		if _, ok := groupZones[simpleGroupKey]; !ok {
			groupZones[simpleGroupKey] = make(map[string]struct{})
		}
		groupZones[simpleGroupKey][zone] = struct{}{}

		for _, poolID := range poolKeys {
			acc, ok := poolStats[poolID]
			if !ok {
				acc = &poolAccumulator{
					pdbNodeCounts: make(map[string]map[string]int),
					pdbSlack:      make(map[string]int32),
				}
				poolStats[poolID] = acc
			}
			acc.utilizationKey = simplePoolID
			if poolID == simplePoolID {
				acc.groupKey = simpleGroupKey
			} else {
				workloadGroupKey := "workload:" + workloadPool
				acc.groupKey = workloadGroupKey
				if _, ok := groupZones[workloadGroupKey]; !ok {
					groupZones[workloadGroupKey] = make(map[string]struct{})
				}
				groupZones[workloadGroupKey][zone] = struct{}{}
			}
			if nodeIsSpot[node.Name] {
				acc.spotNodes++
			} else {
				acc.odNodes++
			}
		}
	}

	// 2. List Pods & PDBs
	pods, err := c.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pdbs, err := c.client.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Warn("failed to list PDBs", "error", err)
	}

	// Map PDBs by namespace (simplified) or label selector (complex, skip for MVP)
	// MVP: Check if namespace has any restricted PDB
	nsRestricted := make(map[string]bool)
	pdbsByNamespace := make(map[string][]compiledPDB)
	if pdbs != nil {
		for _, pdb := range pdbs.Items {
			if pdb.Status.CurrentHealthy <= pdb.Status.DesiredHealthy {
				nsRestricted[pdb.Namespace] = true
			}
			selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err != nil {
				continue
			}
			pdbsByNamespace[pdb.Namespace] = append(pdbsByNamespace[pdb.Namespace], compiledPDB{
				key:                pdb.Namespace + "/" + pdb.Name,
				disruptionsAllowed: pdb.Status.DisruptionsAllowed,
				selector:           selector,
			})
		}
	}

	// 2.5 List ReplicaSets to determine redundancy
	rss, err := c.client.AppsV1().ReplicaSets("").List(ctx, metav1.ListOptions{})
	rsReplicas := make(map[string]int32)
	if err == nil {
		for _, rs := range rss.Items {
			if rs.Spec.Replicas != nil {
				rsReplicas[rs.Name] = *rs.Spec.Replicas
			}
		}
	} else {
		c.logger.Warn("failed to list ReplicaSets", "error", err)
	}

	// 2.6 Fetch pool utilization from metrics provider (if available)
	poolUtilization := make(map[string]float64)
	if c.utilProv != nil {
		if utils, err := c.utilProv.GetPoolUtilization(ctx); err == nil {
			poolUtilization = utils
			c.logger.Debug("fetched pool utilization", "pools", len(utils))
		} else {
			c.logger.Warn("failed to fetch pool utilization", "error", err)
		}
	}

	// 3. Aggregate per Pool
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		poolIDs, ok := nodeToPools[pod.Spec.NodeName]
		if !ok {
			continue // Pod on unknown node?
		}

		// Startup Time (annotation override supported)
		latency := getStartupTimeWithOverride(&pod)
		weight := getPodWeight(&pod)

		// Outage Penalty (annotation override supported, weighted by CPU for inference, MAX for guardrails)
		penalty := getOutagePenaltyWithOverride(&pod, nsRestricted[pod.Namespace], rsReplicas)

		// Priority Score (P0=1.0, P1=0.75, P2=0.5, P3=0.25)
		// weighted by CPU for inference, MAX for guardrails
		pScore := getPriorityScore(&pod)
		isCritical := isCriticalServicePod(&pod, pScore)
		isStateful := isStatefulPod(&pod)
		workloadRelevant := isWorkloadPod(&pod)
		matchedPDB := matchPDBForPod(&pod, pdbsByNamespace[pod.Namespace])
		evictable := workloadRelevant && (matchedPDB == nil || matchedPDB.disruptionsAllowed > 0)

		for _, poolID := range poolIDs {
			acc, exists := poolStats[poolID]
			if !exists {
				acc = &poolAccumulator{
					pdbNodeCounts: make(map[string]map[string]int),
					pdbSlack:      make(map[string]int32),
				}
				poolStats[poolID] = acc
			}
			if latency > 0 {
				acc.latencies = append(acc.latencies, weightedValue{val: latency, weight: weight})
			}
			acc.penalties = append(acc.penalties, weightedValue{val: penalty, weight: weight})
			if penalty > acc.maxPenalty {
				acc.maxPenalty = penalty
			}
			acc.priorities = append(acc.priorities, weightedValue{val: pScore, weight: weight})
			if pScore > acc.maxPriority {
				acc.maxPriority = pScore
			}
			if pScore >= 1.0 {
				acc.hasCriticalPod = true
			}

			if workloadRelevant {
				acc.workloadPods++
				if isStateful {
					acc.statefulPods++
				}
				if evictable {
					acc.evictablePods++
				}
				if isCritical {
					acc.criticalPods++
					if nodeIsSpot[pod.Spec.NodeName] {
						acc.criticalOnSpot++
					}
				}
			}
			if matchedPDB != nil {
				if _, ok := acc.pdbNodeCounts[matchedPDB.key]; !ok {
					acc.pdbNodeCounts[matchedPDB.key] = make(map[string]int)
				}
				acc.pdbNodeCounts[matchedPDB.key][pod.Spec.NodeName]++
				acc.pdbSlack[matchedPDB.key] = matchedPDB.disruptionsAllowed
			}
		}
	}

	// 4. Finalize Features
	newFeatures := make(map[string]WorkloadFeatures)
	for poolID, acc := range poolStats {
		// Weighted P95 Startup
		p95 := 60.0 // Default
		if len(acc.latencies) > 0 {
			p95 = calculateWeightedPercentile(acc.latencies, 0.95)
		}

		// Weighted average Outage Penalty for inference
		// (reflects majority workload behavior, not dominated by single critical pod)
		avgPenalty := 4.0 // P2 default
		if len(acc.penalties) > 0 {
			avgPenalty = calculateWeightedAverage(acc.penalties)
		}

		// Weighted average Priority Score for inference
		avgPriority := 0.5 // P2 default
		if len(acc.priorities) > 0 {
			avgPriority = calculateWeightedAverage(acc.priorities)
		}

		// MAX values for guardrails (safety-critical)
		maxPenalty := acc.maxPenalty
		if maxPenalty == 0 {
			maxPenalty = 4.0 // P2 default
		}
		maxPriority := acc.maxPriority
		if maxPriority == 0 {
			maxPriority = 0.5 // P2 default
		}

		// Get pool utilization (falls back to "default" or 0.5)
		util := 0.5 // Default: assume 50% utilization
		if u, ok := poolUtilization[acc.utilizationKey]; ok {
			util = u
		} else if u, ok := poolUtilization["default"]; ok {
			util = u // Use cluster-wide default if available
		}

		poolSafety := computePoolSafetyVector(acc, util, len(groupZones[acc.groupKey]), p95)

		newFeatures[poolID] = WorkloadFeatures{
			PodStartupTime:     p95,
			OutagePenaltyHours: avgPenalty,  // Weighted avg for inference
			PriorityScore:      avgPriority, // Weighted avg for inference
			ClusterUtilization: util,        // From Prometheus/metrics-server
			PoolSafety:         poolSafety,
			MaxOutagePenalty:   maxPenalty,  // MAX for guardrails
			MaxPriorityScore:   maxPriority, // MAX for guardrails
			HasCriticalPod:     acc.hasCriticalPod,
		}
	}

	c.metrics.PoolFeatures = newFeatures
	c.metrics.LastUpdated = time.Now()

	c.logger.Info("collected workload metrics", "pools", len(newFeatures))
	return &c.metrics, nil
}

// WorkloadPoolLabel is the label key for customer-defined workload pools.
const WorkloadPoolLabel = "spotvortex.io/pool"

// Annotation keys for workload-specific overrides.
// These allow operators to tune SpotVortex behavior per-workload without cluster-wide config.
const (
	// AnnotationOutagePenalty overrides the calculated outage penalty (e.g., "10h", "24h").
	// Use when business impact differs from what PriorityClass implies.
	AnnotationOutagePenalty = "spotvortex.io/outage-penalty"

	// AnnotationStartupTime overrides the observed startup time in seconds (e.g., "600").
	// Use for slow-starting apps (ML models, JVM warmup) that K8s Ready condition underestimates.
	AnnotationStartupTime = "spotvortex.io/startup-time"

	// AnnotationMigrationTier assigns an explicit migration tier (0=critical, 1=standard, 2=batch).
	// Maps to priority scores: 0→1.0, 1→0.5, 2→0.25
	AnnotationMigrationTier = "spotvortex.io/migration-tier"
)

// GetNodePoolID generates the simple "InstanceType:Zone" pool ID.
// Use GetExtendedPoolID when workload pool partitioning is enabled.
func GetNodePoolID(node *corev1.Node) string {
	zone := node.Labels["topology.kubernetes.io/zone"]
	it := node.Labels["node.kubernetes.io/instance-type"]
	if zone == "" {
		zone = "unknown"
	}
	if it == "" {
		it = "unknown"
	}
	return fmt.Sprintf("%s:%s", it, zone)
}

// GetExtendedPoolID generates a pool ID that includes the workload pool label.
// Format: "<workload_pool>:<instance_type>:<zone>" if spotvortex.io/pool exists,
// otherwise falls back to "<instance_type>:<zone>".
// This prevents cross-pool metric mixing in production (Section 2.1 of production doc).
func GetExtendedPoolID(node *corev1.Node) string {
	zone := node.Labels["topology.kubernetes.io/zone"]
	it := node.Labels["node.kubernetes.io/instance-type"]
	pool := node.Labels[WorkloadPoolLabel]

	if zone == "" {
		zone = "unknown"
	}
	if it == "" {
		it = "unknown"
	}

	if pool != "" {
		return fmt.Sprintf("%s:%s:%s", pool, it, zone)
	}
	return fmt.Sprintf("%s:%s", it, zone)
}

// GetPoolFeatures returns features for a given pool
func (c *Collector) GetPoolFeatures(poolID string) WorkloadFeatures {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if f, ok := c.metrics.PoolFeatures[poolID]; ok {
		return f
	}
	c.logger.Warn("pool workload features unavailable; using conservative defaults", "pool", poolID)
	// Fallback Defaults (Phase 4.2)
	return WorkloadFeatures{
		PodStartupTime:     300.0,
		OutagePenaltyHours: 5.0, // Weighted avg for inference
		PriorityScore:      0.5, // P2 - weighted avg for inference
		PoolSafety:         config.DefaultPoolSafetyVector(),
		MaxOutagePenalty:   5.0, // MAX for guardrails
		MaxPriorityScore:   0.5, // P2 - MAX for guardrails
		HasCriticalPod:     false,
	}
}

// Internal Helpers

type weightedValue struct {
	val    float64
	weight float64
}

type poolAccumulator struct {
	latencies []weightedValue

	// Weighted values for penalty and priority (for inference)
	penalties  []weightedValue
	priorities []weightedValue

	// Max values for guardrails
	maxPenalty     float64
	maxPriority    float64
	hasCriticalPod bool // P0/system-critical pod detected

	groupKey       string
	utilizationKey string
	spotNodes      int
	odNodes        int
	workloadPods   int
	statefulPods   int
	evictablePods  int
	criticalPods   int
	criticalOnSpot int
	pdbNodeCounts  map[string]map[string]int
	pdbSlack       map[string]int32
}

type compiledPDB struct {
	key                string
	disruptionsAllowed int32
	selector           labels.Selector
}

func getPodStartupLatency(pod *corev1.Pod) float64 {
	if pod.Status.StartTime == nil {
		return 0
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return cond.LastTransitionTime.Sub(pod.Status.StartTime.Time).Seconds()
		}
	}
	return 0
}

func getPodWeight(pod *corev1.Pod) float64 {
	// Weight by CPU request
	cpu := float64(0)
	for _, c := range pod.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().AsApproximateFloat64()
	}
	if cpu <= 0 {
		return 1.0
	}
	return cpu
}

// calculateWeightedPercentile computes P-th percentile (0-1)
func calculateWeightedPercentile(values []weightedValue, p float64) float64 {
	sort.Slice(values, func(i, j int) bool {
		return values[i].val < values[j].val
	})

	totalWeight := 0.0
	for _, v := range values {
		totalWeight += v.weight
	}

	target := totalWeight * p
	current := 0.0
	for _, v := range values {
		current += v.weight
		if current >= target {
			return v.val
		}
	}
	return values[len(values)-1].val
}

// calculateWeightedAverage computes weighted average of values.
// Used for OutagePenalty and PriorityScore so that a single critical pod
// doesn't dominate the entire pool's inference behavior.
func calculateWeightedAverage(values []weightedValue) float64 {
	if len(values) == 0 {
		return 0
	}

	totalWeight := 0.0
	weightedSum := 0.0
	for _, v := range values {
		weightedSum += v.val * v.weight
		totalWeight += v.weight
	}

	if totalWeight == 0 {
		return 0
	}
	return weightedSum / totalWeight
}

func computePoolSafetyVector(acc *poolAccumulator, util float64, zoneCount int, restartP95Seconds float64) config.PoolSafetyVector {
	vector := config.DefaultPoolSafetyVector()
	if acc == nil {
		return vector
	}

	util = clampUnit(util)
	vector.RestartP95Seconds = restartP95Seconds
	vector.SpareODHeadroomNodes = math.Max(0, float64(acc.odNodes)*(1.0-util))
	vector.ZoneDiversificationScore = computeZoneDiversificationScore(zoneCount)

	if acc.workloadPods == 0 {
		return config.NormalizePoolSafetyVector(vector)
	}

	if acc.criticalPods > 0 {
		vector.CriticalServiceSpotConcentration = float64(acc.criticalOnSpot) / float64(acc.criticalPods)
	}
	if acc.workloadPods > 0 {
		vector.StatefulPodFraction = float64(acc.statefulPods) / float64(acc.workloadPods)
		vector.EvictablePodFraction = float64(acc.evictablePods) / float64(acc.workloadPods)
	}
	vector.MinPDBSlackIfOneNodeLost, vector.MinPDBSlackIfTwoNodesLost = computePDBSlackBounds(acc)
	vector.RecoveryBudgetViolationRisk = computeRecoveryBudgetViolationRisk(vector)
	vector.SafeMaxSpotRatio = computeSafeMaxSpotRatio(vector)
	return config.NormalizePoolSafetyVector(vector)
}

func computePDBSlackBounds(acc *poolAccumulator) (float64, float64) {
	if acc == nil || len(acc.pdbNodeCounts) == 0 {
		return 0, 0
	}
	minOne := math.Inf(1)
	minTwo := math.Inf(1)

	for pdbKey, nodeCounts := range acc.pdbNodeCounts {
		allowed := float64(acc.pdbSlack[pdbKey])
		var maxOne, maxTwo int
		for _, count := range nodeCounts {
			if count >= maxOne {
				maxTwo = maxOne
				maxOne = count
				continue
			}
			if count > maxTwo {
				maxTwo = count
			}
		}

		slackOne := allowed - float64(maxOne)
		slackTwo := allowed - float64(maxOne+maxTwo)
		if slackOne < minOne {
			minOne = slackOne
		}
		if slackTwo < minTwo {
			minTwo = slackTwo
		}
	}

	if math.IsInf(minOne, 1) {
		minOne = 0
	}
	if math.IsInf(minTwo, 1) {
		minTwo = 0
	}
	return minOne, minTwo
}

func computeZoneDiversificationScore(zoneCount int) float64 {
	switch {
	case zoneCount >= 3:
		return 1.0
	case zoneCount == 2:
		return 0.5
	case zoneCount == 1:
		return 0.0
	default:
		return 0.0
	}
}

func computeRecoveryBudgetViolationRisk(v config.PoolSafetyVector) float64 {
	risk := 0.0

	if v.MinPDBSlackIfOneNodeLost < 0 {
		risk = math.Max(risk, 0.95)
	} else if v.MinPDBSlackIfOneNodeLost < 1 {
		risk = math.Max(risk, 0.70)
	}

	if v.MinPDBSlackIfTwoNodesLost < 0 {
		risk = math.Max(risk, 0.85)
	} else if v.MinPDBSlackIfTwoNodesLost < 1 {
		risk = math.Max(risk, 0.55)
	}

	if v.CriticalServiceSpotConcentration > 0 {
		risk = math.Max(risk, clampUnit(0.20+0.80*v.CriticalServiceSpotConcentration))
	}
	if v.StatefulPodFraction > 0 {
		risk = math.Max(risk, clampUnit(0.15+0.65*v.StatefulPodFraction))
	}
	if v.RestartP95Seconds >= 600 {
		risk = math.Max(risk, 0.75)
	} else if v.RestartP95Seconds >= 300 {
		risk = math.Max(risk, 0.50)
	} else if v.RestartP95Seconds >= 120 {
		risk = math.Max(risk, 0.30)
	}
	if v.SpareODHeadroomNodes < 1 {
		risk = math.Max(risk, 0.55)
	}
	risk = math.Max(risk, clampUnit((1.0-v.ZoneDiversificationScore)*0.60))
	risk = math.Max(risk, clampUnit((1.0-v.EvictablePodFraction)*0.85))

	return clampUnit(risk)
}

func computeSafeMaxSpotRatio(v config.PoolSafetyVector) float64 {
	cap := 1.0

	switch {
	case v.CriticalServiceSpotConcentration >= 0.80:
		cap = math.Min(cap, 0.10)
	case v.CriticalServiceSpotConcentration >= 0.50:
		cap = math.Min(cap, 0.25)
	case v.CriticalServiceSpotConcentration >= 0.25:
		cap = math.Min(cap, 0.50)
	}

	switch {
	case v.MinPDBSlackIfOneNodeLost < 0:
		cap = math.Min(cap, 0.10)
	case v.MinPDBSlackIfOneNodeLost < 1:
		cap = math.Min(cap, 0.35)
	}

	switch {
	case v.MinPDBSlackIfTwoNodesLost < 0:
		cap = math.Min(cap, 0.25)
	case v.MinPDBSlackIfTwoNodesLost < 1:
		cap = math.Min(cap, 0.50)
	}

	switch {
	case v.StatefulPodFraction >= 0.50:
		cap = math.Min(cap, 0.50)
	case v.StatefulPodFraction >= 0.25:
		cap = math.Min(cap, 0.70)
	}

	switch {
	case v.RestartP95Seconds >= 600:
		cap = math.Min(cap, 0.25)
	case v.RestartP95Seconds >= 300:
		cap = math.Min(cap, 0.50)
	}

	switch {
	case v.RecoveryBudgetViolationRisk >= 0.90:
		cap = math.Min(cap, 0.10)
	case v.RecoveryBudgetViolationRisk >= 0.75:
		cap = math.Min(cap, 0.25)
	case v.RecoveryBudgetViolationRisk >= 0.60:
		cap = math.Min(cap, 0.40)
	}

	if v.SpareODHeadroomNodes < 1 {
		cap = math.Min(cap, 0.60)
	}
	if v.ZoneDiversificationScore < 0.50 {
		cap = math.Min(cap, 0.60)
	}
	if v.EvictablePodFraction < 0.25 {
		cap = math.Min(cap, 0.20)
	} else if v.EvictablePodFraction < 0.50 {
		cap = math.Min(cap, 0.40)
	}

	return clampUnit(cap)
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func isStatefulPod(pod *corev1.Pod) bool {
	for _, own := range pod.OwnerReferences {
		if own.Kind == "StatefulSet" {
			return true
		}
	}
	return false
}

func isCriticalServicePod(pod *corev1.Pod, priorityScore float64) bool {
	if pod != nil && pod.Annotations["spotvortex.io/critical"] == "true" {
		return true
	}
	return priorityScore >= 0.75
}

func isWorkloadPod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, own := range pod.OwnerReferences {
		if own.Kind == "DaemonSet" {
			return false
		}
	}
	return pod.Spec.NodeName != ""
}

func matchPDBForPod(pod *corev1.Pod, pdbs []compiledPDB) *compiledPDB {
	if pod == nil {
		return nil
	}
	for _, pdb := range pdbs {
		if pdb.selector == nil || pdb.selector.Empty() {
			continue
		}
		if pdb.selector.Matches(labels.Set(pod.Labels)) {
			match := pdb
			return &match
		}
	}
	return nil
}

func calculateOutagePenalty(pod *corev1.Pod, restricted bool, rsReplicas map[string]int32) float64 {
	// Logic from Gap Analysis:
	// P0=48h, P1=12h, P2=4h, P3=1h
	// If replicas >= 2 and PDB allows eviction, halve the penalty.
	// If PDB restricted (restricted=true) -> Double penalty

	// Determine Priority
	pc := pod.Spec.PriorityClassName
	base := 4.0 // P2
	if strings.Contains(pc, "system-node-critical") || strings.Contains(pc, "system-cluster-critical") {
		base = 48.0 // P0
	} else if strings.Contains(pc, "high") {
		base = 12.0 // P1
	} else if strings.Contains(pc, "low") {
		base = 1.0 // P3
	}

	if restricted {
		base *= 2.0
	}

	// Simplify: Assume stateless if not StatefulSet (checking owner refs)
	isStateful := false
	replicas := int32(1)
	for _, own := range pod.OwnerReferences {
		if own.Kind == "StatefulSet" {
			isStateful = true
		}
		if own.Kind == "ReplicaSet" {
			if r, ok := rsReplicas[own.Name]; ok {
				replicas = r
			}
		}
	}
	if isStateful {
		base *= 2.0
	} else if replicas >= 2 && !restricted {
		// Redundancy bonus if not restricted
		base *= 0.5
	}

	return base
}

func getPriorityScore(pod *corev1.Pod) float64 {
	// Check for annotation override first
	if tier, ok := pod.Annotations[AnnotationMigrationTier]; ok {
		switch tier {
		case "0":
			return 1.0 // Critical
		case "1":
			return 0.5 // Standard
		case "2":
			return 0.25 // Batch
		}
	}

	// P0=1.0, P1=0.75, P2=0.5, P3=0.25
	// Using same heuristic as penalty
	pc := pod.Spec.PriorityClassName
	if strings.Contains(pc, "system") {
		return 1.0
	} else if strings.Contains(pc, "high") {
		return 0.75
	} else if strings.Contains(pc, "low") {
		return 0.25
	}
	return 0.5 // P2
}

// getOutagePenaltyWithOverride returns outage penalty, checking annotation override first.
func getOutagePenaltyWithOverride(pod *corev1.Pod, restricted bool, rsReplicas map[string]int32) float64 {
	// Check for annotation override first
	if penaltyStr, ok := pod.Annotations[AnnotationOutagePenalty]; ok {
		if penalty := parseHoursDuration(penaltyStr); penalty > 0 {
			return penalty
		}
	}
	// Fall back to calculated penalty
	return calculateOutagePenalty(pod, restricted, rsReplicas)
}

// getStartupTimeWithOverride returns startup time, checking annotation override first.
func getStartupTimeWithOverride(pod *corev1.Pod) float64 {
	// Check for annotation override first
	if startupStr, ok := pod.Annotations[AnnotationStartupTime]; ok {
		var startup float64
		if _, err := fmt.Sscanf(startupStr, "%f", &startup); err == nil && startup > 0 {
			return startup
		}
	}
	// Fall back to observed latency
	return getPodStartupLatency(pod)
}

// parseHoursDuration parses a duration string like "10h", "24h", "0.5h" into hours.
func parseHoursDuration(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Check for "h" suffix
	if strings.HasSuffix(s, "h") {
		var hours float64
		if _, err := fmt.Sscanf(s[:len(s)-1], "%f", &hours); err == nil {
			return hours
		}
	}

	// Try parsing as plain number (assumed hours)
	var hours float64
	if _, err := fmt.Sscanf(s, "%f", &hours); err == nil {
		return hours
	}

	return 0
}
