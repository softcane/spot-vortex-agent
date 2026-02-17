package collector

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetNodePoolID(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
	}{
		{
			name: "full labels",
			labels: map[string]string{
				"node.kubernetes.io/instance-type": "c5.2xlarge",
				"topology.kubernetes.io/zone":      "us-east-1a",
			},
			expected: "c5.2xlarge:us-east-1a",
		},
		{
			name:     "missing labels",
			labels:   nil,
			expected: "unknown:unknown",
		},
		{
			name: "missing zone",
			labels: map[string]string{
				"node.kubernetes.io/instance-type": "m5.large",
			},
			expected: "m5.large:unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: tc.labels,
				},
			}
			got := GetNodePoolID(node)
			if got != tc.expected {
				t.Errorf("GetNodePoolID: got %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestGetExtendedPoolID(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
	}{
		{
			name: "with workload pool label",
			labels: map[string]string{
				"spotvortex.io/pool":               "core-services",
				"node.kubernetes.io/instance-type": "c5.2xlarge",
				"topology.kubernetes.io/zone":      "us-east-1a",
			},
			expected: "core-services:c5.2xlarge:us-east-1a",
		},
		{
			name: "without workload pool label (backward compat)",
			labels: map[string]string{
				"node.kubernetes.io/instance-type": "c5.2xlarge",
				"topology.kubernetes.io/zone":      "us-east-1a",
			},
			expected: "c5.2xlarge:us-east-1a",
		},
		{
			name: "batch pool",
			labels: map[string]string{
				"spotvortex.io/pool":               "batch",
				"node.kubernetes.io/instance-type": "m5.4xlarge",
				"topology.kubernetes.io/zone":      "us-west-2b",
			},
			expected: "batch:m5.4xlarge:us-west-2b",
		},
		{
			name:     "no labels",
			labels:   nil,
			expected: "unknown:unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: tc.labels,
				},
			}
			got := GetExtendedPoolID(node)
			if got != tc.expected {
				t.Errorf("GetExtendedPoolID: got %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestWorkloadPoolLabelConstant(t *testing.T) {
	// Verify the constant matches what's documented
	if WorkloadPoolLabel != "spotvortex.io/pool" {
		t.Errorf("WorkloadPoolLabel: got %q, want %q",
			WorkloadPoolLabel, "spotvortex.io/pool")
	}
}

func TestCalculateWeightedAverage(t *testing.T) {
	tests := []struct {
		name     string
		values   []weightedValue
		expected float64
	}{
		{
			name:     "empty",
			values:   nil,
			expected: 0,
		},
		{
			name: "single value",
			values: []weightedValue{
				{val: 10.0, weight: 1.0},
			},
			expected: 10.0,
		},
		{
			name: "equal weights",
			values: []weightedValue{
				{val: 10.0, weight: 1.0},
				{val: 20.0, weight: 1.0},
			},
			expected: 15.0,
		},
		{
			name: "weighted by CPU - single critical pod doesn't dominate",
			// Scenario: 9 normal pods (P2, 4h penalty, 1 CPU each) + 1 critical pod (P0, 48h penalty, 1 CPU)
			// Old MAX approach: 48h (dominated by single critical pod)
			// New weighted avg: (9*4 + 1*48) / 10 = 8.4h (reflects majority)
			values: []weightedValue{
				{val: 4.0, weight: 1.0}, // Pod 1: P2
				{val: 4.0, weight: 1.0}, // Pod 2: P2
				{val: 4.0, weight: 1.0}, // Pod 3: P2
				{val: 4.0, weight: 1.0}, // Pod 4: P2
				{val: 4.0, weight: 1.0}, // Pod 5: P2
				{val: 4.0, weight: 1.0}, // Pod 6: P2
				{val: 4.0, weight: 1.0}, // Pod 7: P2
				{val: 4.0, weight: 1.0}, // Pod 8: P2
				{val: 4.0, weight: 1.0}, // Pod 9: P2
				{val: 48.0, weight: 1.0}, // Pod 10: P0 (critical)
			},
			expected: 8.4,
		},
		{
			name: "weighted by CPU - heavy pods influence more",
			// Scenario: 1 heavy pod (4 CPU, 48h penalty) + 3 light pods (1 CPU each, 4h penalty)
			// Weighted avg: (4*48 + 3*4) / 7 = 204/7 ≈ 29.14h
			values: []weightedValue{
				{val: 48.0, weight: 4.0}, // Heavy pod (P0)
				{val: 4.0, weight: 1.0},  // Light pod 1 (P2)
				{val: 4.0, weight: 1.0},  // Light pod 2 (P2)
				{val: 4.0, weight: 1.0},  // Light pod 3 (P2)
			},
			expected: 204.0 / 7.0,
		},
		{
			name: "zero total weight",
			values: []weightedValue{
				{val: 10.0, weight: 0.0},
				{val: 20.0, weight: 0.0},
			},
			expected: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := calculateWeightedAverage(tc.values)
			// Use tolerance for float comparison
			diff := got - tc.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > 0.01 {
				t.Errorf("calculateWeightedAverage: got %.4f, want %.4f", got, tc.expected)
			}
		})
	}
}

func TestWeightedAggregationVsMax(t *testing.T) {
	// Verify that the weighted approach produces different results than MAX
	// when there's a single outlier (critical pod)
	values := []weightedValue{
		{val: 4.0, weight: 1.0},  // 9x P2 pods
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 4.0, weight: 1.0},
		{val: 48.0, weight: 1.0}, // 1x P0 pod (critical)
	}

	// Calculate weighted average (for inference)
	weightedAvg := calculateWeightedAverage(values)

	// Calculate max (for guardrails)
	maxVal := 0.0
	for _, v := range values {
		if v.val > maxVal {
			maxVal = v.val
		}
	}

	// Weighted average should be much lower than MAX
	if weightedAvg >= maxVal {
		t.Errorf("weighted avg (%.2f) should be less than max (%.2f)", weightedAvg, maxVal)
	}

	// Weighted avg should reflect majority (closer to 4.0 than 48.0)
	if weightedAvg > 12.0 {
		t.Errorf("weighted avg (%.2f) too high - should reflect majority P2 pods", weightedAvg)
	}

	// Max should be the critical pod's value
	if maxVal != 48.0 {
		t.Errorf("max should be 48.0 (critical pod), got %.2f", maxVal)
	}
}

func TestParseHoursDuration(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected float64
	}{
		{name: "hours with h suffix", input: "10h", expected: 10.0},
		{name: "decimal hours", input: "0.5h", expected: 0.5},
		{name: "large hours", input: "48h", expected: 48.0},
		{name: "plain number", input: "24", expected: 24.0},
		{name: "decimal without suffix", input: "2.5", expected: 2.5},
		{name: "empty string", input: "", expected: 0},
		{name: "whitespace", input: "  10h  ", expected: 10.0},
		{name: "invalid format", input: "abc", expected: 0},
		{name: "negative", input: "-5h", expected: -5.0}, // Edge case - should still parse
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHoursDuration(tc.input)
			if got != tc.expected {
				t.Errorf("parseHoursDuration(%q): got %.2f, want %.2f", tc.input, got, tc.expected)
			}
		})
	}
}

func TestAnnotationConstants(t *testing.T) {
	// Verify annotation constants match documented values
	tests := []struct {
		name     string
		constant string
		expected string
	}{
		{"OutagePenalty", AnnotationOutagePenalty, "spotvortex.io/outage-penalty"},
		{"StartupTime", AnnotationStartupTime, "spotvortex.io/startup-time"},
		{"MigrationTier", AnnotationMigrationTier, "spotvortex.io/migration-tier"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.constant != tc.expected {
				t.Errorf("%s: got %q, want %q", tc.name, tc.constant, tc.expected)
			}
		})
	}
}

func TestGetPriorityScoreWithMigrationTier(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		priority    string
		expected    float64
	}{
		{
			name:        "tier 0 (critical)",
			annotations: map[string]string{AnnotationMigrationTier: "0"},
			expected:    1.0,
		},
		{
			name:        "tier 1 (standard)",
			annotations: map[string]string{AnnotationMigrationTier: "1"},
			expected:    0.5,
		},
		{
			name:        "tier 2 (batch)",
			annotations: map[string]string{AnnotationMigrationTier: "2"},
			expected:    0.25,
		},
		{
			name:        "no annotation - falls back to priority class",
			annotations: nil,
			priority:    "system-node-critical",
			expected:    1.0,
		},
		{
			name:        "no annotation - high priority",
			annotations: nil,
			priority:    "high-priority",
			expected:    0.75,
		},
		{
			name:        "no annotation - low priority",
			annotations: nil,
			priority:    "low-priority",
			expected:    0.25,
		},
		{
			name:        "no annotation - default",
			annotations: nil,
			priority:    "",
			expected:    0.5,
		},
		{
			name:        "annotation overrides priority class",
			annotations: map[string]string{AnnotationMigrationTier: "2"},
			priority:    "system-node-critical", // Would be 1.0 without annotation
			expected:    0.25,                    // Annotation wins
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tc.annotations,
				},
				Spec: corev1.PodSpec{
					PriorityClassName: tc.priority,
				},
			}
			got := getPriorityScore(pod)
			if got != tc.expected {
				t.Errorf("getPriorityScore: got %.2f, want %.2f", got, tc.expected)
			}
		})
	}
}

func TestGetStartupTimeWithOverride(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    float64
	}{
		{
			name:        "annotation override",
			annotations: map[string]string{AnnotationStartupTime: "600"},
			expected:    600.0,
		},
		{
			name:        "annotation with decimal",
			annotations: map[string]string{AnnotationStartupTime: "300.5"},
			expected:    300.5,
		},
		{
			name:        "invalid annotation - falls back to 0 (no status)",
			annotations: map[string]string{AnnotationStartupTime: "invalid"},
			expected:    0, // No pod status = 0
		},
		{
			name:        "no annotation - falls back to 0 (no status)",
			annotations: nil,
			expected:    0, // No pod status = 0
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tc.annotations,
				},
			}
			got := getStartupTimeWithOverride(pod)
			if got != tc.expected {
				t.Errorf("getStartupTimeWithOverride: got %.2f, want %.2f", got, tc.expected)
			}
		})
	}
}

func TestGetOutagePenaltyWithOverride(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		priority    string
		expected    float64
	}{
		{
			name:        "annotation override 10h",
			annotations: map[string]string{AnnotationOutagePenalty: "10h"},
			expected:    10.0,
		},
		{
			name:        "annotation override 24h",
			annotations: map[string]string{AnnotationOutagePenalty: "24"},
			expected:    24.0,
		},
		{
			name:        "no annotation - P0 system critical",
			annotations: nil,
			priority:    "system-node-critical",
			expected:    48.0,
		},
		{
			name:        "no annotation - P1 high",
			annotations: nil,
			priority:    "high-priority",
			expected:    12.0,
		},
		{
			name:        "no annotation - P2 default",
			annotations: nil,
			priority:    "",
			expected:    4.0,
		},
		{
			name:        "no annotation - P3 low",
			annotations: nil,
			priority:    "low-priority",
			expected:    1.0,
		},
		{
			name:        "annotation overrides priority class",
			annotations: map[string]string{AnnotationOutagePenalty: "2h"},
			priority:    "system-node-critical", // Would be 48h without annotation
			expected:    2.0,                     // Annotation wins
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tc.annotations,
				},
				Spec: corev1.PodSpec{
					PriorityClassName: tc.priority,
				},
			}
			got := getOutagePenaltyWithOverride(pod, false, nil)
			if got != tc.expected {
				t.Errorf("getOutagePenaltyWithOverride: got %.2f, want %.2f", got, tc.expected)
			}
		})
	}
}
