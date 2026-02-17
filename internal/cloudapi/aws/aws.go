// Package aws provides AWS EC2 Spot Price API implementation.
// Uses aws-sdk-go-v2 (2026 SDK) for real API calls.
// No mocks, no fallbacks - production only.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

const (
	// CacheTTL is the duration to cache spot prices (per phase.md: 5 minutes)
	CacheTTL = 5 * time.Minute

	// HistorySteps is the number of 5-minute steps to keep (24 = 2 hours)
	HistorySteps = 24

	// PriceHistoryLookback is how far back to query spot price history
	PriceHistoryLookback = 2 * time.Hour
)

// SpotPriceData contains current and historical spot price information.
type SpotPriceData struct {
	CurrentPrice  float64
	OnDemandPrice float64
	PriceHistory  []float64 // Last 24 steps (2 hours)
	Volatility    float64   // Rolling std dev
	LastUpdated   time.Time
	InstanceType  string
	Zone          string
}

// PriceClient provides real AWS spot price data.
type PriceClient struct {
	ec2Client     *ec2.Client
	pricingClient *pricing.Client
	logger        *slog.Logger
	region        string

	mu            sync.RWMutex
	cache         map[string]*SpotPriceData // key: instanceType:zone
	onDemandCache map[string]float64        // key: instanceType
}

// NewPriceClient creates a new AWS price client.
func NewPriceClient(ctx context.Context, region string, logger *slog.Logger) (*PriceClient, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &PriceClient{
		ec2Client: ec2.NewFromConfig(cfg),
		pricingClient: pricing.NewFromConfig(cfg, func(o *pricing.Options) {
			// Pricing API is only available in us-east-1
			o.Region = "us-east-1"
		}),
		logger:        logger,
		region:        region,
		cache:         make(map[string]*SpotPriceData),
		onDemandCache: make(map[string]float64),
	}, nil
}

// GetSpotPrice returns spot price data for the given instance type and zone.
// Uses 5-minute TTL cache per phase.md specification.
func (c *PriceClient) GetSpotPrice(ctx context.Context, instanceType, zone string) (*SpotPriceData, error) {
	cacheKey := instanceType + ":" + zone

	// Check cache first
	c.mu.RLock()
	if cached, ok := c.cache[cacheKey]; ok {
		if time.Since(cached.LastUpdated) < CacheTTL {
			c.mu.RUnlock()
			return cached, nil
		}
	}
	c.mu.RUnlock()

	// Cache miss or expired - fetch from AWS
	c.logger.Debug("fetching spot price from AWS",
		"instance_type", instanceType,
		"zone", zone,
	)

	startTime := time.Now().Add(-PriceHistoryLookback)

	input := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(instanceType)},
		AvailabilityZone:    aws.String(zone),
		StartTime:           aws.Time(startTime),
		ProductDescriptions: []string{"Linux/UNIX"},
		MaxResults:          aws.Int32(100),
	}

	result, err := c.ec2Client.DescribeSpotPriceHistory(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe spot price history: %w", err)
	}

	if len(result.SpotPriceHistory) == 0 {
		return nil, fmt.Errorf("no spot price history found for %s in %s", instanceType, zone)
	}

	// Parse price history
	priceHistory := c.buildPriceHistory(result.SpotPriceHistory)
	currentPrice := priceHistory[len(priceHistory)-1]
	volatility := c.calculateVolatility(priceHistory)

	// Get on-demand price
	onDemandPrice, err := c.GetOnDemandPrice(ctx, instanceType)
	if err != nil {
		return nil, fmt.Errorf("failed to get on-demand price for %s: %w", instanceType, err)
	}

	data := &SpotPriceData{
		CurrentPrice:  currentPrice,
		OnDemandPrice: onDemandPrice,
		PriceHistory:  priceHistory,
		Volatility:    volatility,
		LastUpdated:   time.Now(),
		InstanceType:  instanceType,
		Zone:          zone,
	}

	// Update cache
	c.mu.Lock()
	c.cache[cacheKey] = data
	c.mu.Unlock()

	c.logger.Info("spot price updated",
		"instance_type", instanceType,
		"zone", zone,
		"current_price", currentPrice,
		"ondemand_price", onDemandPrice,
		"volatility", volatility,
	)

	return data, nil
}

// GetOnDemandPrice fetches the on-demand price for an instance type.
func (c *PriceClient) GetOnDemandPrice(ctx context.Context, instanceType string) (float64, error) {
	// Check cache
	c.mu.RLock()
	if price, ok := c.onDemandCache[instanceType]; ok {
		c.mu.RUnlock()
		return price, nil
	}
	c.mu.RUnlock()

	// Use AWS Pricing API
	// Filter for on-demand, Linux, current region
	input := &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			{
				Type:  pricingtypes.FilterTypeTermMatch,
				Field: aws.String("instanceType"),
				Value: aws.String(instanceType),
			},
			{
				Type:  pricingtypes.FilterTypeTermMatch,
				Field: aws.String("operatingSystem"),
				Value: aws.String("Linux"),
			},
			{
				Type:  pricingtypes.FilterTypeTermMatch,
				Field: aws.String("preInstalledSw"),
				Value: aws.String("NA"),
			},
			{
				Type:  pricingtypes.FilterTypeTermMatch,
				Field: aws.String("tenancy"),
				Value: aws.String("Shared"),
			},
			{
				Type:  pricingtypes.FilterTypeTermMatch,
				Field: aws.String("capacitystatus"),
				Value: aws.String("Used"),
			},
			{
				Type:  pricingtypes.FilterTypeTermMatch,
				Field: aws.String("regionCode"),
				Value: aws.String(c.region),
			},
		},
		MaxResults: aws.Int32(1),
	}

	result, err := c.pricingClient.GetProducts(ctx, input)
	if err != nil {
		return 0, fmt.Errorf("failed to get products: %w", err)
	}

	if len(result.PriceList) == 0 {
		return 0, fmt.Errorf("no pricing found for %s", instanceType)
	}

	// Parse the complex JSON response to extract hourly price
	price, err := parseOnDemandPrice(result.PriceList[0])
	if err != nil {
		return 0, err
	}

	// Cache the result
	c.mu.Lock()
	c.onDemandCache[instanceType] = price
	c.mu.Unlock()

	return price, nil
}

// buildPriceHistory converts AWS spot price history to 24-step array.
func (c *PriceClient) buildPriceHistory(history []types.SpotPrice) []float64 {
	if len(history) == 0 {
		return nil
	}

	// AWS returns newest first, we want oldest first
	prices := make([]float64, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].SpotPrice != nil {
			price := parsePrice(*history[i].SpotPrice)
			prices = append(prices, price)
		}
	}

	// Pad or truncate to HistorySteps
	if len(prices) < HistorySteps {
		// Pad with first available price
		padding := make([]float64, HistorySteps-len(prices))
		for i := range padding {
			padding[i] = prices[0]
		}
		prices = append(padding, prices...)
	} else if len(prices) > HistorySteps {
		prices = prices[len(prices)-HistorySteps:]
	}

	return prices
}

// calculateVolatility computes rolling standard deviation of prices.
func (c *PriceClient) calculateVolatility(prices []float64) float64 {
	if len(prices) < 2 {
		return 0
	}

	// Calculate mean
	var sum float64
	for _, p := range prices {
		sum += p
	}
	mean := sum / float64(len(prices))

	// Calculate variance
	var variance float64
	for _, p := range prices {
		diff := p - mean
		variance += diff * diff
	}
	variance /= float64(len(prices) - 1)

	return math.Sqrt(variance)
}

// parsePrice converts AWS price string to float64.
func parsePrice(s string) float64 {
	var price float64
	fmt.Sscanf(s, "%f", &price)
	return price
}

// parseOnDemandPrice extracts hourly price from AWS Pricing API response.
func parseOnDemandPrice(priceList string) (float64, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(priceList), &payload); err != nil {
		return 0, fmt.Errorf("failed to parse pricing payload: %w", err)
	}

	termsAny, ok := payload["terms"]
	if !ok {
		return 0, fmt.Errorf("pricing payload missing terms")
	}
	terms, ok := termsAny.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("invalid terms format in pricing payload")
	}
	onDemandAny, ok := terms["OnDemand"]
	if !ok {
		return 0, fmt.Errorf("pricing payload missing terms.OnDemand")
	}
	onDemand, ok := onDemandAny.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("invalid OnDemand format in pricing payload")
	}

	best := 0.0
	found := false

	parseUSD := func(v interface{}) (float64, bool) {
		switch val := v.(type) {
		case string:
			p, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			if err != nil || p <= 0 {
				return 0, false
			}
			return p, true
		case float64:
			if val <= 0 {
				return 0, false
			}
			return val, true
		default:
			return 0, false
		}
	}

	for _, skuAny := range onDemand {
		skuMap, ok := skuAny.(map[string]interface{})
		if !ok {
			continue
		}
		for _, termAny := range skuMap {
			termMap, ok := termAny.(map[string]interface{})
			if !ok {
				continue
			}
			dimsAny, ok := termMap["priceDimensions"]
			if !ok {
				continue
			}
			dimsMap, ok := dimsAny.(map[string]interface{})
			if !ok {
				continue
			}
			for _, dimAny := range dimsMap {
				dimMap, ok := dimAny.(map[string]interface{})
				if !ok {
					continue
				}
				ppuAny, ok := dimMap["pricePerUnit"]
				if !ok {
					continue
				}
				ppuMap, ok := ppuAny.(map[string]interface{})
				if !ok {
					continue
				}
				usdAny, ok := ppuMap["USD"]
				if !ok {
					continue
				}
				price, ok := parseUSD(usdAny)
				if !ok {
					continue
				}
				if !found || price < best {
					best = price
					found = true
				}
			}
		}
	}

	if !found {
		return 0, fmt.Errorf("unable to extract USD on-demand price from payload")
	}
	return best, nil
}
