package capacity

import "testing"

func TestNormalizeCapacityType(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "spot", input: "spot", want: "spot"},
		{name: "eks spot", input: "SPOT", want: "spot"},
		{name: "lifecycle ec2spot", input: "Ec2Spot", want: "spot"},
		{name: "ondemand", input: "ON_DEMAND", want: "on-demand"},
		{name: "normal", input: "normal", want: "on-demand"},
		{name: "reserved", input: "capacity_block", want: "reserved"},
		{name: "unknown", input: "mystery", want: "mystery"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeCapacityType(tc.input); got != tc.want {
				t.Fatalf("NormalizeCapacityType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCapacityTypeFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "karpenter wins",
			labels: map[string]string{
				LabelKarpenterCapacity: "on-demand",
				LabelNodeLifecycle:     "spot",
			},
			want: "on-demand",
		},
		{
			name: "spotvortex capacity label",
			labels: map[string]string{
				LabelSpotVortexCapacity: "spot",
			},
			want: "spot",
		},
		{
			name: "eks capacity label",
			labels: map[string]string{
				LabelEKSCapacityType: "SPOT",
			},
			want: "spot",
		},
		{
			name: "lifecycle fallback",
			labels: map[string]string{
				LabelNodeLifecycle: "Ec2Spot",
			},
			want: "spot",
		},
		{
			name: "missing",
			labels: map[string]string{
				"app": "demo",
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapacityTypeFromLabels(tc.labels); got != tc.want {
				t.Fatalf("CapacityTypeFromLabels() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsSpotLabels(t *testing.T) {
	if !IsSpotLabels(map[string]string{LabelEKSCapacityType: "SPOT"}) {
		t.Fatal("expected EKS SPOT label to resolve as spot")
	}
	if IsSpotLabels(map[string]string{LabelSpotVortexCapacity: "on-demand"}) {
		t.Fatal("expected on-demand label to resolve as non-spot")
	}
}
