package cloudapi

import (
	"context"
	"strings"
	"testing"
)

func TestFakePriceProvider_DeterministicSequenceAndRepeatLast(t *testing.T) {
	provider, err := NewFakePriceProviderFromJSON(`{
  "default": {
    "current_price": 0.20,
    "on_demand_price": 1.00,
    "price_history": [0.20, 0.20]
  },
  "series": {
    "m5.large:us-east-1a": [
      {"current_price": 0.25, "on_demand_price": 1.10, "price_history": [0.24, 0.25]},
      {"current_price": 0.90, "on_demand_price": 1.10, "price_history": [0.70, 0.90]}
    ]
  },
  "repeat_last": true
}`)
	if err != nil {
		t.Fatalf("NewFakePriceProviderFromJSON failed: %v", err)
	}

	ctx := context.Background()

	od1, err := provider.GetOnDemandPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetOnDemandPrice failed: %v", err)
	}
	if od1 != 1.10 {
		t.Fatalf("on-demand=%v, want 1.10", od1)
	}

	spot1, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice(1) failed: %v", err)
	}
	if spot1.CurrentPrice != 0.25 {
		t.Fatalf("spot(1)=%v, want 0.25", spot1.CurrentPrice)
	}
	if len(spot1.PriceHistory) != 2 || spot1.PriceHistory[1] != 0.25 {
		t.Fatalf("unexpected history(1): %#v", spot1.PriceHistory)
	}

	spot2, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice(2) failed: %v", err)
	}
	if spot2.CurrentPrice != 0.90 {
		t.Fatalf("spot(2)=%v, want 0.90", spot2.CurrentPrice)
	}

	spot3, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a")
	if err != nil {
		t.Fatalf("GetSpotPrice(3) failed: %v", err)
	}
	if spot3.CurrentPrice != 0.90 {
		t.Fatalf("spot(3)=%v, want repeat-last 0.90", spot3.CurrentPrice)
	}
}

func TestFakePriceProvider_WildcardPrecedence(t *testing.T) {
	provider, err := NewFakePriceProviderFromJSON(`{
  "default": {"current_price": 0.11, "on_demand_price": 0.91},
  "series": {
    "*:*": [{"current_price": 0.12, "on_demand_price": 0.92}],
    "*:us-east-1a": [{"current_price": 0.13, "on_demand_price": 0.93}],
    "m5.large:*": [{"current_price": 0.14, "on_demand_price": 0.94}],
    "m5.large:us-east-1a": [{"current_price": 0.15, "on_demand_price": 0.95}]
  }
}`)
	if err != nil {
		t.Fatalf("NewFakePriceProviderFromJSON failed: %v", err)
	}

	ctx := context.Background()
	tests := []struct {
		name         string
		instanceType string
		zone         string
		wantSpot     float64
	}{
		{
			name:         "exact match",
			instanceType: "m5.large",
			zone:         "us-east-1a",
			wantSpot:     0.15,
		},
		{
			name:         "instance wildcard",
			instanceType: "m5.large",
			zone:         "us-west-2b",
			wantSpot:     0.14,
		},
		{
			name:         "zone wildcard",
			instanceType: "c6i.large",
			zone:         "us-east-1a",
			wantSpot:     0.13,
		},
		{
			name:         "global wildcard",
			instanceType: "c6i.large",
			zone:         "eu-west-1a",
			wantSpot:     0.12,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spot, spotErr := provider.GetSpotPrice(ctx, tc.instanceType, tc.zone)
			if spotErr != nil {
				t.Fatalf("GetSpotPrice failed: %v", spotErr)
			}
			if spot.CurrentPrice != tc.wantSpot {
				t.Fatalf("spot=%v, want %v", spot.CurrentPrice, tc.wantSpot)
			}
		})
	}
}

func TestFakePriceProvider_ErrorInjectionAndExhaustion(t *testing.T) {
	provider, err := NewFakePriceProviderFromJSON(`{
  "default": {"current_price": 0.20, "on_demand_price": 1.00},
  "series": {
    "m5.large:us-east-1a": [
      {"current_price": 0.25, "on_demand_price": 1.10},
      {"error": "throttled"}
    ]
  },
  "repeat_last": false
}`)
	if err != nil {
		t.Fatalf("NewFakePriceProviderFromJSON failed: %v", err)
	}

	ctx := context.Background()

	if _, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a"); err != nil {
		t.Fatalf("GetSpotPrice(1) unexpected error: %v", err)
	}

	if _, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a"); err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("expected injected throttled error, got: %v", err)
	}

	if _, err := provider.GetSpotPrice(ctx, "m5.large", "us-east-1a"); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected exhaustion error, got: %v", err)
	}
}

func TestFakePriceProvider_Validation(t *testing.T) {
	_, err := NewFakePriceProvider(FakePriceScenario{})
	if err == nil {
		t.Fatal("expected empty scenario to fail validation")
	}

	_, err = NewFakePriceProvider(FakePriceScenario{
		Series: map[string][]FakePricePoint{
			"bad-key": {{CurrentPrice: ptrFloat(0.2)}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("expected key format validation error, got: %v", err)
	}

	_, err = NewFakePriceProvider(FakePriceScenario{
		Series: map[string][]FakePricePoint{
			"m5.large:us-east-1a": {},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "at least one step") {
		t.Fatalf("expected empty sequence validation error, got: %v", err)
	}
}

func ptrFloat(v float64) *float64 {
	return &v
}
