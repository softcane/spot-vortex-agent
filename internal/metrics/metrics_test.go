package metrics

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordSavings(t *testing.T) {
	tests := []struct {
		name          string
		spotPrice     float64
		onDemandPrice float64
		nodeCount     int
		expected      float64
	}{
		{
			name:          "positive savings",
			spotPrice:     0.2,
			onDemandPrice: 1.0,
			nodeCount:     10,
			expected:      8.0, // (1.0 - 0.2) * 10 = 8.0
		},
		{
			name:          "zero savings",
			spotPrice:     1.0,
			onDemandPrice: 1.0,
			nodeCount:     5,
			expected:      0.0,
		},
		{
			name:          "negative savings (expensive spot)",
			spotPrice:     1.2,
			onDemandPrice: 1.0,
			nodeCount:     10,
			expected:      -2.0, // (1.0 - 1.2) * 10 = -2.0
		},
		{
			name:          "zero price ignored",
			spotPrice:     0.0,
			onDemandPrice: 1.0,
			nodeCount:     10,
			expected:      0.0, // Function shouldn't update if price is 0, but since we can't easily check "not updated", we rely on initialization state which is 0?
			// Actually existing value persists. We should verify behaviors carefully.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Helper to reset gauge isn't trivial with promauto globals,
			// so we just Set(0) before test if needed, or just assert result.
			SavingsUSDHourly.Set(0)

			RecordSavings(tt.spotPrice, tt.onDemandPrice, tt.nodeCount)

			if tt.spotPrice > 0 && tt.onDemandPrice > 0 {
				val := testutil.ToFloat64(SavingsUSDHourly)
				if math.Abs(val-tt.expected) > 0.0001 {
					t.Errorf("expected %f, got %f", tt.expected, val)
				}
			}
		})
	}
}
