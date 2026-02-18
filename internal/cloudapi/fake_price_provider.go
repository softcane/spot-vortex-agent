package cloudapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// FakePriceScenario describes deterministic spot/on-demand price responses
// for local tests and e2e harnesses.
type FakePriceScenario struct {
	Default    FakePricePoint              `json:"default"`
	Series     map[string][]FakePricePoint `json:"series"`
	RepeatLast *bool                       `json:"repeat_last,omitempty"`
}

// FakePricePoint defines one scripted response step.
// Pointer fields allow explicit zero values while still supporting fallback.
type FakePricePoint struct {
	CurrentPrice  *float64   `json:"current_price,omitempty"`
	OnDemandPrice *float64   `json:"on_demand_price,omitempty"`
	PriceHistory  *[]float64 `json:"price_history,omitempty"`
	Volatility    *float64   `json:"volatility,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// FakePriceProvider is a deterministic, script-driven PriceProvider for tests.
type FakePriceProvider struct {
	mu         sync.Mutex
	scenario   FakePriceScenario
	repeatLast bool
	cursors    map[string]int
}

// NewFakePriceProvider builds a fake provider from an in-memory scenario.
func NewFakePriceProvider(scenario FakePriceScenario) (*FakePriceProvider, error) {
	if err := validateFakePriceScenario(scenario); err != nil {
		return nil, err
	}
	repeatLast := true
	if scenario.RepeatLast != nil {
		repeatLast = *scenario.RepeatLast
	}
	return &FakePriceProvider{
		scenario:   scenario,
		repeatLast: repeatLast,
		cursors:    make(map[string]int, len(scenario.Series)),
	}, nil
}

// NewFakePriceProviderFromFile loads a fake provider from a JSON file.
func NewFakePriceProviderFromFile(path string) (*FakePriceProvider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fake price scenario file %q: %w", path, err)
	}
	return NewFakePriceProviderFromJSONBytes(raw)
}

// NewFakePriceProviderFromJSON loads a fake provider from JSON text.
func NewFakePriceProviderFromJSON(raw string) (*FakePriceProvider, error) {
	return NewFakePriceProviderFromJSONBytes([]byte(raw))
}

// NewFakePriceProviderFromJSONBytes loads a fake provider from JSON bytes.
func NewFakePriceProviderFromJSONBytes(raw []byte) (*FakePriceProvider, error) {
	var scenario FakePriceScenario
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scenario); err != nil {
		return nil, fmt.Errorf("decode fake price scenario json: %w", err)
	}
	return NewFakePriceProvider(scenario)
}

// GetSpotPrice returns scripted spot price data for instance/zone.
func (f *FakePriceProvider) GetSpotPrice(ctx context.Context, instanceType, zone string) (SpotPriceData, error) {
	_ = ctx
	return f.resolveSpotPrice(instanceType, zone, true)
}

// GetOnDemandPrice returns scripted on-demand price for instance/zone.
// This call does not advance scripted sequence state.
func (f *FakePriceProvider) GetOnDemandPrice(ctx context.Context, instanceType, zone string) (float64, error) {
	_ = ctx
	data, err := f.resolveSpotPrice(instanceType, zone, false)
	if err != nil {
		return 0, err
	}
	return data.OnDemandPrice, nil
}

func (f *FakePriceProvider) resolveSpotPrice(instanceType, zone string, advance bool) (SpotPriceData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	step, err := f.nextStepLocked(instanceType, zone, advance)
	if err != nil {
		return SpotPriceData{}, err
	}

	if step.Error != "" {
		return SpotPriceData{}, fmt.Errorf("fake price provider injected error for %s:%s: %s", instanceType, zone, step.Error)
	}

	return SpotPriceData{
		CurrentPrice:  derefFloat(step.CurrentPrice),
		OnDemandPrice: derefFloat(step.OnDemandPrice),
		PriceHistory:  cloneHistory(step.PriceHistory),
		Volatility:    derefFloat(step.Volatility),
		InstanceType:  instanceType,
		Zone:          zone,
	}, nil
}

func (f *FakePriceProvider) nextStepLocked(instanceType, zone string, advance bool) (FakePricePoint, error) {
	seriesKey, ok := f.selectSeriesKey(instanceType, zone)
	if !ok {
		if !f.scenario.Default.hasAnyValue() {
			return FakePricePoint{}, fmt.Errorf("no fake price series or default for %s:%s", instanceType, zone)
		}
		return f.scenario.Default, nil
	}

	sequence := f.scenario.Series[seriesKey]
	if len(sequence) == 0 {
		return FakePricePoint{}, fmt.Errorf("empty fake price series %q", seriesKey)
	}

	index := f.cursors[seriesKey]
	if index >= len(sequence) {
		if !f.repeatLast {
			return FakePricePoint{}, fmt.Errorf("fake price series exhausted for %q", seriesKey)
		}
		index = len(sequence) - 1
	}

	if advance && index < len(sequence)-1 {
		f.cursors[seriesKey] = index + 1
	} else if advance && index == len(sequence)-1 && !f.repeatLast {
		// Mark exhausted so the next call returns an exhaustion error.
		f.cursors[seriesKey] = len(sequence)
	}

	step := mergeFakePricePoints(f.scenario.Default, sequence[index])
	return step, nil
}

func (f *FakePriceProvider) selectSeriesKey(instanceType, zone string) (string, bool) {
	instanceType = strings.TrimSpace(instanceType)
	zone = strings.TrimSpace(zone)
	if instanceType == "" {
		instanceType = "*"
	}
	if zone == "" {
		zone = "*"
	}

	candidates := []string{
		instanceType + ":" + zone,
		instanceType + ":*",
		"*:" + zone,
		"*:*",
	}
	for _, key := range candidates {
		if _, ok := f.scenario.Series[key]; ok {
			return key, true
		}
	}
	return "", false
}

func validateFakePriceScenario(scenario FakePriceScenario) error {
	if !scenario.Default.hasAnyValue() && len(scenario.Series) == 0 {
		return fmt.Errorf("fake price scenario must define default and/or series")
	}

	for key, sequence := range scenario.Series {
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("fake price scenario contains empty series key")
		}
		if !strings.Contains(key, ":") {
			return fmt.Errorf("fake price series key %q must be in <instanceType>:<zone> format", key)
		}
		if len(sequence) == 0 {
			return fmt.Errorf("fake price series %q must contain at least one step", key)
		}
	}
	return nil
}

func mergeFakePricePoints(base, override FakePricePoint) FakePricePoint {
	out := base
	if override.CurrentPrice != nil {
		out.CurrentPrice = override.CurrentPrice
	}
	if override.OnDemandPrice != nil {
		out.OnDemandPrice = override.OnDemandPrice
	}
	if override.PriceHistory != nil {
		out.PriceHistory = override.PriceHistory
	}
	if override.Volatility != nil {
		out.Volatility = override.Volatility
	}
	if override.Error != "" {
		out.Error = override.Error
	}
	return out
}

func (p FakePricePoint) hasAnyValue() bool {
	return p.CurrentPrice != nil ||
		p.OnDemandPrice != nil ||
		p.PriceHistory != nil ||
		p.Volatility != nil ||
		p.Error != ""
}

func cloneHistory(history *[]float64) []float64 {
	if history == nil {
		return nil
	}
	out := make([]float64, len(*history))
	copy(out, *history)
	return out
}

func derefFloat(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
