// Package audit provides the Sovereign Auditor for SpotVortex.
//
// Generates cryptographically signed savings manifests locally.
// Based on: architecture.md (Sovereign Auditor calculates savings in VPC)
package audit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// SavingsManifest is the signed proof of value sent to the SaaS.
// This is the ONLY data that leaves the customer VPC.
type SavingsManifest struct {
	ClusterID     string    `json:"cluster_id"`
	NodeID        string    `json:"node_id"`
	InstanceType  string    `json:"instance_type"`
	Region        string    `json:"region"`
	Zone          string    `json:"zone"`
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	OnDemandPrice float64   `json:"on_demand_price_hourly"`
	SpotPrice     float64   `json:"spot_price_hourly"`
	DurationHours float64   `json:"duration_hours"`
	TotalSaved    float64   `json:"total_saved_usd"`
	Signature     string    `json:"signature"`
}

// Config for the Auditor
type Config struct {
	SecretKey string // HMAC key for signing manifests
	ClusterID string // Unique cluster identifier
	DryRun    bool   // If true, don't send to SaaS
}

// Auditor generates signed savings manifests
type Auditor struct {
	client kubernetes.Interface
	config Config
	logger *slog.Logger
}

// NewAuditor creates a new Sovereign Auditor
func NewAuditor(client kubernetes.Interface, config Config, logger *slog.Logger) *Auditor {
	return &Auditor{
		client: client,
		config: config,
		logger: logger,
	}
}

// GenerateManifest creates a signed savings manifest for a node
func (a *Auditor) GenerateManifest(node *corev1.Node, startTime time.Time) (*SavingsManifest, error) {
	endTime := time.Now()
	duration := endTime.Sub(startTime).Hours()

	// Get prices from node labels (set by SpotVortex agent)
	odPrice := getFloatLabel(node, "spotvortex.io/od-price")
	spotPrice := getFloatLabel(node, "spotvortex.io/spot-price")

	if odPrice == 0 {
		return nil, fmt.Errorf("on-demand price label missing on node %s", node.Name)
	}
	if spotPrice == 0 {
		return nil, fmt.Errorf("spot price label missing on node %s", node.Name)
	}

	// Calculate savings
	totalSaved := (odPrice - spotPrice) * duration
	if totalSaved < 0 {
		totalSaved = 0 // Spot can't cost more than OD
	}

	// Get zone/region from node labels
	zone := node.Labels["topology.kubernetes.io/zone"]
	if zone == "" {
		zone = node.Labels["failure-domain.beta.kubernetes.io/zone"]
	}
	region := node.Labels["topology.kubernetes.io/region"]
	if region == "" {
		region = node.Labels["failure-domain.beta.kubernetes.io/region"]
	}

	manifest := &SavingsManifest{
		ClusterID:     a.config.ClusterID,
		NodeID:        node.Name,
		InstanceType:  node.Labels["node.kubernetes.io/instance-type"],
		Region:        region,
		Zone:          zone,
		StartTime:     startTime,
		EndTime:       endTime,
		OnDemandPrice: odPrice,
		SpotPrice:     spotPrice,
		DurationHours: duration,
		TotalSaved:    totalSaved,
	}

	// Sign the manifest for integrity
	manifest.Signature = a.signManifest(manifest)

	a.logger.Info("generated savings manifest",
		"node", node.Name,
		"saved", totalSaved,
		"duration_hours", duration,
	)

	return manifest, nil
}

// signManifest creates HMAC-SHA256 signature
func (a *Auditor) signManifest(m *SavingsManifest) string {
	// Create deterministic payload
	payload := fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%.6f",
		m.ClusterID,
		m.NodeID,
		m.StartTime.UTC().Format(time.RFC3339),
		m.OnDemandPrice,
		m.SpotPrice,
		m.TotalSaved,
	)

	h := hmac.New(sha256.New, []byte(a.config.SecretKey))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyManifest checks if a manifest signature is valid
func (a *Auditor) VerifyManifest(m *SavingsManifest) bool {
	expected := a.signManifest(m)
	return hmac.Equal([]byte(expected), []byte(m.Signature))
}

// ToJSON serializes manifest to JSON
func (m *SavingsManifest) ToJSON() ([]byte, error) {
	return json.Marshal(m)
}

// getFloatLabel safely extracts a float from node labels
func getFloatLabel(node *corev1.Node, key string) float64 {
	if node.Labels == nil {
		return 0
	}
	val, ok := node.Labels[key]
	if !ok {
		return 0
	}
	var f float64
	_, err := fmt.Sscanf(val, "%f", &f)
	if err != nil {
		return 0
	}
	return f
}
