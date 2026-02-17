// Package cloudapi provides factory for cloud provider instantiation.
package cloudapi

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/pradeepsingh/spot-vortex-agent/internal/cloudapi/aws"
	"github.com/pradeepsingh/spot-vortex-agent/internal/cloudapi/gcp"
)

// PriceProvider defines the interface for spot price retrieval.
type PriceProvider interface {
	// GetSpotPrice returns current spot/preemptible price data.
	GetSpotPrice(ctx context.Context, instanceType, zone string) (SpotPriceData, error)

	// GetOnDemandPrice returns the on-demand price for an instance type.
	GetOnDemandPrice(ctx context.Context, instanceType, zone string) (float64, error)
}

// SpotPriceData is a unified representation of spot price data across clouds.
type SpotPriceData struct {
	CurrentPrice  float64
	OnDemandPrice float64
	PriceHistory  []float64
	Volatility    float64
	InstanceType  string
	Zone          string
}

// NewAutoDetectedPriceProvider creates a price provider based on detected cloud.
func NewAutoDetectedPriceProvider(ctx context.Context, logger *slog.Logger) (PriceProvider, CloudType, error) {
	cloud := DetectCloud(ctx)

	switch cloud {
	case CloudTypeAWS:
		region := getAWSRegion()
		client, err := aws.NewPriceClient(ctx, region, logger)
		if err != nil {
			return nil, cloud, fmt.Errorf("failed to create AWS price client: %w", err)
		}
		return &awsPriceProviderAdapter{client: client}, cloud, nil

	case CloudTypeGCP:
		project := getGCPProject()
		client, err := gcp.NewPriceClient(ctx, project, logger)
		if err != nil {
			return nil, cloud, fmt.Errorf("failed to create GCP price client: %w", err)
		}
		return &gcpPriceProviderAdapter{client: client}, cloud, nil

	default:
		return nil, cloud, fmt.Errorf("unsupported cloud: %s", cloud)
	}
}

// NewAWSPriceProvider creates an AWS-backed price provider for a specific region.
func NewAWSPriceProvider(ctx context.Context, region string, logger *slog.Logger) (PriceProvider, error) {
	client, err := aws.NewPriceClient(ctx, region, logger)
	if err != nil {
		return nil, err
	}
	return &awsPriceProviderAdapter{client: client}, nil
}

// NewGCPPriceProvider creates a GCP-backed price provider for a specific project.
func NewGCPPriceProvider(ctx context.Context, project string, logger *slog.Logger) (PriceProvider, error) {
	client, err := gcp.NewPriceClient(ctx, project, logger)
	if err != nil {
		return nil, err
	}
	return &gcpPriceProviderAdapter{client: client}, nil
}

// getAWSRegion returns the AWS region from environment or default.
func getAWSRegion() string {
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region
	}
	if region := os.Getenv("AWS_DEFAULT_REGION"); region != "" {
		return region
	}
	return "us-east-1"
}

// getGCPProject returns the GCP project from environment.
func getGCPProject() string {
	if project := os.Getenv("GOOGLE_CLOUD_PROJECT"); project != "" {
		return project
	}
	if project := os.Getenv("GCP_PROJECT"); project != "" {
		return project
	}
	return ""
}

// awsPriceProviderAdapter adapts AWS PriceClient to PriceProvider.
type awsPriceProviderAdapter struct {
	client *aws.PriceClient
}

func (a *awsPriceProviderAdapter) GetSpotPrice(ctx context.Context, instanceType, zone string) (SpotPriceData, error) {
	data, err := a.client.GetSpotPrice(ctx, instanceType, zone)
	if err != nil {
		return SpotPriceData{}, err
	}
	return SpotPriceData{
		CurrentPrice:  data.CurrentPrice,
		OnDemandPrice: data.OnDemandPrice,
		PriceHistory:  data.PriceHistory,
		Volatility:    data.Volatility,
		InstanceType:  data.InstanceType,
		Zone:          data.Zone,
	}, nil
}

func (a *awsPriceProviderAdapter) GetOnDemandPrice(ctx context.Context, instanceType, zone string) (float64, error) {
	return a.client.GetOnDemandPrice(ctx, instanceType)
}

// gcpPriceProviderAdapter adapts GCP PriceClient to PriceProvider.
type gcpPriceProviderAdapter struct {
	client *gcp.PriceClient
}

func (g *gcpPriceProviderAdapter) GetSpotPrice(ctx context.Context, machineType, zone string) (SpotPriceData, error) {
	data, err := g.client.GetSpotPrice(ctx, machineType, zone)
	if err != nil {
		return SpotPriceData{}, err
	}
	return SpotPriceData{
		CurrentPrice:  data.CurrentPrice,
		OnDemandPrice: data.OnDemandPrice,
		PriceHistory:  data.PriceHistory,
		Volatility:    data.Volatility,
		InstanceType:  data.MachineType,
		Zone:          data.Zone,
	}, nil
}

func (g *gcpPriceProviderAdapter) GetOnDemandPrice(ctx context.Context, machineType, zone string) (float64, error) {
	return g.client.GetOnDemandPrice(ctx, machineType, zone)
}
