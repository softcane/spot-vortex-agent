package controller

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/pradeepsingh/spot-vortex-agent/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type nodeInfo struct {
	name         string
	isSpot       bool
	isFake       bool
	isManaged    bool
	isControl    bool
	zone         string
	instanceType string
	workloadPool string // spotvortex.io/pool label for extended pool ID
}

func (c *Controller) nodeInfoMap(ctx context.Context) (map[string]nodeInfo, error) {
	if c.k8s == nil {
		return nil, fmt.Errorf("k8s client required")
	}
	nodes, err := c.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	info := make(map[string]nodeInfo, len(nodes.Items))
	for _, node := range nodes.Items {
		labels := node.Labels
		capacityType := ""
		if labels != nil {
			capacityType = labels["karpenter.sh/capacity-type"]
		}
		isFake := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == "spotvortex.io/fake" {
				isFake = true
				break
			}
		}

		isManaged := false
		isControl := false
		if labels != nil {
			isManaged = labels["spotvortex.io/managed"] == "true"
			if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
				isControl = true
			}
			if _, ok := labels["node-role.kubernetes.io/master"]; ok {
				isControl = true
			}
		}

		base := nodeInfo{
			name:         node.Name,
			isSpot:       capacityType == "spot",
			isFake:       isFake,
			isManaged:    isManaged,
			isControl:    isControl,
			zone:         labelValue(labels, "topology.kubernetes.io/zone", "unknown"),
			instanceType: labelValue(labels, "node.kubernetes.io/instance-type", "unknown"),
			workloadPool: labelValue(labels, "spotvortex.io/pool", ""),
		}
		info[node.Name] = base
		for _, addr := range node.Status.Addresses {
			address := strings.TrimSpace(addr.Address)
			if address == "" {
				continue
			}
			info[address] = base
			info[address+":9100"] = base
		}
	}
	return info, nil
}

func (c *Controller) syntheticNodeMetrics(ctx context.Context) ([]metrics.NodeMetrics, error) {
	if c.k8s == nil {
		return nil, fmt.Errorf("k8s client required")
	}
	if c.metric == nil {
		c.metric = newMetricSynth(time.Now().UnixNano() + 2)
	}

	nodes, err := c.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	now := time.Now()
	results := make([]metrics.NodeMetrics, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		labels := node.Labels
		capacityType := ""
		if labels != nil {
			capacityType = labels["karpenter.sh/capacity-type"]
		}

		cpu, mem := c.metric.Next(node.Name)
		results = append(results, metrics.NodeMetrics{
			NodeID:             node.Name,
			Zone:               labelValue(labels, "topology.kubernetes.io/zone", "unknown"),
			InstanceType:       labelValue(labels, "node.kubernetes.io/instance-type", "unknown"),
			CPUUsagePercent:    cpu,
			MemoryUsagePercent: mem,
			IsSpot:             capacityType == "spot",
			Timestamp:          now,
		})
	}

	return results, nil
}

func labelValue(labels map[string]string, key, fallback string) string {
	if labels == nil {
		return fallback
	}
	if value, ok := labels[key]; ok && value != "" {
		return value
	}
	return fallback
}

type metricSynth struct {
	mu  sync.Mutex
	rng *rand.Rand
	cpu map[string]float64
	mem map[string]float64
}

func newMetricSynth(seed int64) *metricSynth {
	return &metricSynth{
		rng: rand.New(rand.NewSource(seed)),
		cpu: make(map[string]float64),
		mem: make(map[string]float64),
	}
}

func (m *metricSynth) Next(nodeID string) (float64, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cpu := m.cpu[nodeID]
	mem := m.mem[nodeID]
	if cpu == 0 {
		cpu = 20 + m.rng.Float64()*40
	}
	if mem == 0 {
		mem = 20 + m.rng.Float64()*40
	}

	cpu = clampFloat(cpu+(m.rng.Float64()*10-5), 5, 95)
	mem = clampFloat(mem+(m.rng.Float64()*10-5), 5, 95)

	m.cpu[nodeID] = cpu
	m.mem[nodeID] = mem

	return cpu, mem
}

type priceSynth struct {
	mu       sync.Mutex
	rng      *rand.Rand
	last     map[string]float64
	history  map[string][]float64
	ondemand map[string]float64
	minPrice float64
	maxPrice float64
}

func newPriceSynth(seed int64) *priceSynth {
	return &priceSynth{
		rng:      rand.New(rand.NewSource(seed)),
		last:     make(map[string]float64),
		history:  make(map[string][]float64),
		ondemand: make(map[string]float64),
		minPrice: 0.10,
		maxPrice: 3.00,
	}
}

func (p *priceSynth) Next(nodeID string, isSpot bool) (float64, float64, []float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ondemand := p.ondemand[nodeID]
	if ondemand == 0 {
		ondemand = 1.2 + p.rng.Float64()*1.0
		p.ondemand[nodeID] = ondemand
	}

	price := p.last[nodeID]
	if price == 0 {
		price = ondemand * (0.35 + p.rng.Float64()*0.3)
	} else {
		price *= 1.0 + (p.rng.Float64()*0.1 - 0.05)
	}

	spikeChance := 0.05
	if isSpot {
		spikeChance = 0.15
	}
	if p.rng.Float64() < spikeChance {
		price = ondemand * (0.8 + p.rng.Float64()*0.3)
	}

	if price > ondemand {
		price = ondemand * 0.98
	}

	price = clampFloat(price, p.minPrice, minFloat(p.maxPrice, ondemand))
	p.last[nodeID] = price

	history := append(p.history[nodeID], price)
	if len(history) > 24 {
		history = history[len(history)-24:]
	}
	p.history[nodeID] = history

	return price, ondemand, append([]float64(nil), history...)
}

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
