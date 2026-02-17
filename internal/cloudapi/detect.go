// Package cloudapi provides cloud provider auto-detection.
// Detects AWS, GCP, or Azure based on environment and IMDS.
package cloudapi

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

// CloudType represents a cloud provider.
type CloudType string

const (
	CloudTypeAWS     CloudType = "aws"
	CloudTypeGCP     CloudType = "gcp"
	CloudTypeAzure   CloudType = "azure"
	CloudTypeUnknown CloudType = "unknown"
)

// IMDSEndpoints for cloud detection.
const (
	awsIMDSEndpoint   = "http://169.254.169.254/latest/meta-data/"
	gcpIMDSEndpoint   = "http://metadata.google.internal/computeMetadata/v1/"
	azureIMDSEndpoint = "http://169.254.169.254/metadata/instance"
)

// DetectCloud automatically detects the cloud provider.
// Detection order:
// 1. Environment variables (fastest)
// 2. IMDS endpoints (most reliable)
// 3. Node labels (if k8s client provided)
func DetectCloud(ctx context.Context) CloudType {
	// 1. Check environment variables first (fastest)
	if cloud := detectFromEnv(); cloud != CloudTypeUnknown {
		return cloud
	}

	// 2. Check IMDS endpoints
	if cloud := detectFromIMDS(ctx); cloud != CloudTypeUnknown {
		return cloud
	}

	return CloudTypeUnknown
}

// detectFromEnv checks common environment variables.
func detectFromEnv() CloudType {
	// AWS indicators
	if os.Getenv("AWS_REGION") != "" || os.Getenv("AWS_DEFAULT_REGION") != "" {
		return CloudTypeAWS
	}
	if os.Getenv("AWS_EXECUTION_ENV") != "" {
		return CloudTypeAWS
	}

	// GCP indicators
	if os.Getenv("GOOGLE_CLOUD_PROJECT") != "" || os.Getenv("GCP_PROJECT") != "" {
		return CloudTypeGCP
	}
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		return CloudTypeGCP
	}

	// Azure indicators
	if os.Getenv("AZURE_SUBSCRIPTION_ID") != "" {
		return CloudTypeAzure
	}
	if os.Getenv("AZURE_TENANT_ID") != "" {
		return CloudTypeAzure
	}

	return CloudTypeUnknown
}

// detectFromIMDS probes instance metadata endpoints.
func detectFromIMDS(ctx context.Context) CloudType {
	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	// Check GCP first (has unique header requirement)
	if checkGCPIMDS(ctx, client) {
		return CloudTypeGCP
	}

	// Check AWS
	if checkAWSIMDS(ctx, client) {
		return CloudTypeAWS
	}

	// Check Azure
	if checkAzureIMDS(ctx, client) {
		return CloudTypeAzure
	}

	return CloudTypeUnknown
}

// checkAWSIMDS probes AWS Instance Metadata Service.
func checkAWSIMDS(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", awsIMDSEndpoint, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// checkGCPIMDS probes GCP Metadata Server.
func checkGCPIMDS(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", gcpIMDSEndpoint+"project/project-id", nil)
	if err != nil {
		return false
	}
	// GCP requires this header
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// checkAzureIMDS probes Azure Instance Metadata Service.
func checkAzureIMDS(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", azureIMDSEndpoint+"?api-version=2021-02-01", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Metadata", "true")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// DetectCloudFromNodeLabels detects cloud from Kubernetes node labels.
// Common patterns:
// - AWS: topology.kubernetes.io/region starts with "us-east-", "eu-west-", etc.
// - GCP: topology.kubernetes.io/region contains "us-central1", "europe-west1", etc.
// - Azure: topology.kubernetes.io/region contains "eastus", "westeurope", etc.
func DetectCloudFromNodeLabels(labels map[string]string) CloudType {
	region := labels["topology.kubernetes.io/region"]
	zone := labels["topology.kubernetes.io/zone"]

	// GCP zones are like: us-central1-a, europe-west1-b
	if strings.Contains(zone, "-") && len(strings.Split(zone, "-")) == 3 {
		// Could be GCP or AWS - check region format
		if strings.Contains(region, "central") || strings.Contains(region, "west") || strings.Contains(region, "east") {
			// AWS regions: us-east-1, eu-west-1
			// GCP regions: us-central1, europe-west1
			if strings.HasSuffix(region, "1") || strings.HasSuffix(region, "2") || strings.HasSuffix(region, "3") {
				return CloudTypeGCP
			}
		}
	}

	// AWS zones are like: us-east-1a, eu-west-1b
	if len(zone) > 0 && zone[len(zone)-1] >= 'a' && zone[len(zone)-1] <= 'z' {
		// Last char is letter - likely AWS
		regionPart := zone[:len(zone)-1]
		if strings.HasSuffix(regionPart, "1") || strings.HasSuffix(regionPart, "2") || strings.HasSuffix(regionPart, "3") {
			return CloudTypeAWS
		}
	}

	// Azure zones are just numbers: 1, 2, 3
	if len(zone) == 1 && zone[0] >= '1' && zone[0] <= '3' {
		return CloudTypeAzure
	}

	// Check provider-specific labels
	if _, ok := labels["eks.amazonaws.com/nodegroup"]; ok {
		return CloudTypeAWS
	}
	if _, ok := labels["cloud.google.com/gke-nodepool"]; ok {
		return CloudTypeGCP
	}
	if _, ok := labels["kubernetes.azure.com/agentpool"]; ok {
		return CloudTypeAzure
	}

	return CloudTypeUnknown
}
