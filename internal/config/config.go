// Package config provides configuration loading for SpotVortex.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all SpotVortex configuration.
type Config struct {
	Controller  ControllerConfig  `yaml:"controller"`
	Inference   InferenceConfig   `yaml:"inference"`
	Prometheus  PrometheusConfig  `yaml:"prometheus"`
	Karpenter   KarpenterConfig   `yaml:"karpenter"`
	Autoscaling AutoscalingConfig `yaml:"autoscaling"`
	AWS         AWSConfig         `yaml:"aws"`
	GCP         GCPConfig         `yaml:"gcp"`
}

// KarpenterConfig configures Karpenter integration per PRODUCTION_FLOW_EKS_KARPENTER.md.
type KarpenterConfig struct {
	// Enabled enables Karpenter NodePool weight steering.
	// When true, SpotVortex will patch NodePool weights to steer new provisioning.
	Enabled bool `yaml:"enabled"`

	// UseExtendedPoolID enables extended pool ID format: "<workload_pool>:<instance_type>:<zone>".
	// Requires nodes to have the spotvortex.io/pool label.
	// When false, uses simple format: "<instance_type>:<zone>".
	UseExtendedPoolID bool `yaml:"useExtendedPoolId"`

	// SpotNodePoolSuffix is appended to workload pool name to derive the spot NodePool name.
	// Example: if workload pool is "core-services" and suffix is "-spot", NodePool is "core-services-spot".
	SpotNodePoolSuffix string `yaml:"spotNodePoolSuffix"`

	// OnDemandNodePoolSuffix is appended to workload pool name to derive the on-demand NodePool name.
	// Example: if workload pool is "core-services" and suffix is "-od", NodePool is "core-services-od".
	OnDemandNodePoolSuffix string `yaml:"onDemandNodePoolSuffix"`

	// SpotWeight is the weight to set on the spot NodePool when favoring spot provisioning.
	// Default: 100
	SpotWeight int32 `yaml:"spotWeight"`

	// OnDemandWeight is the weight to set on the on-demand NodePool when favoring on-demand provisioning.
	// Default: 10
	OnDemandWeight int32 `yaml:"onDemandWeight"`

	// ManagedWorkloadPools is an explicit allowlist of workload pools that SpotVortex is allowed to manage.
	// If empty, all workload pools with spotvortex.io/pool label are managed (default behavior).
	// If specified, only pools in this list will have their weights adjusted.
	// Per Section 2.4: "SpotVortex only touches objects it owns (labels or allowlists)".
	ManagedWorkloadPools []string `yaml:"managedWorkloadPools"`

	// WeightChangeCooldownSeconds is the minimum interval between weight changes for a NodePool.
	// Prevents rapid oscillation of weights. Per Section 2.4: "patch weights slowly (hysteresis + cooldown)".
	// Default: 60 seconds.
	WeightChangeCooldownSeconds int `yaml:"weightChangeCooldownSeconds"`

	// UsePoolLevelInference enables per-pool inference aggregation (Section 6 Option 2).
	// When true, inference runs once per pool using the dominant instance type by count.
	// This provides "one coherent action per pool per tick" and is recommended for production.
	// When false, inference runs per node (current behavior).
	UsePoolLevelInference bool `yaml:"usePoolLevelInference"`

	// RespectDisruptionBudgets enables Karpenter disruption budget awareness (Section 2.4).
	// When true, SpotVortex will read NodePool.spec.disruption.budgets and keep drain
	// concurrency below those limits to avoid blocking Karpenter's consolidation/drift.
	// Default: true when Karpenter is enabled.
	RespectDisruptionBudgets bool `yaml:"respectDisruptionBudgets"`
}

// IsWorkloadPoolManaged checks if a workload pool is in the managed allowlist.
// Returns true if the allowlist is empty (all pools managed) or if the pool is in the list.
func (k *KarpenterConfig) IsWorkloadPoolManaged(workloadPool string) bool {
	if len(k.ManagedWorkloadPools) == 0 {
		return true // Empty allowlist = all pools managed
	}
	for _, p := range k.ManagedWorkloadPools {
		if p == workloadPool {
			return true
		}
	}
	return false
}

// WeightChangeCooldown returns the weight change cooldown as a duration.
func (k *KarpenterConfig) WeightChangeCooldown() time.Duration {
	if k.WeightChangeCooldownSeconds <= 0 {
		return 60 * time.Second // Default 60 seconds
	}
	return time.Duration(k.WeightChangeCooldownSeconds) * time.Second
}

// ControllerConfig configures the reconciliation controller.
type ControllerConfig struct {
	RiskThreshold            float64 `yaml:"riskThreshold"`
	MaxDrainRatio            float64 `yaml:"maxDrainRatio"`
	ReconcileIntervalSeconds int     `yaml:"reconcileIntervalSeconds"`
	ConfidenceThreshold      float64 `yaml:"confidenceThreshold"`
	DrainGracePeriodSeconds  int     `yaml:"drainGracePeriodSeconds"`
}

// InferenceConfig configures the ONNX inference engine.
type InferenceConfig struct {
	TFTModelPath      string `yaml:"tftModelPath"`
	RLModelPath       string `yaml:"rlModelPath"`
	ModelManifestPath string `yaml:"modelManifestPath"`
	ExpectedCloud     string `yaml:"expectedCloud"`
}

// PrometheusConfig configures the Prometheus client.
type PrometheusConfig struct {
	URL            string `yaml:"url"`
	TimeoutSeconds int    `yaml:"timeoutSeconds"`
}

// AWSConfig configures AWS spot-price provider settings.
type AWSConfig struct {
	Region string `yaml:"region"`
	// Optional catalog hints for pricing workflows. Not used for model-scope gating.
	InstanceTypes []string `yaml:"instanceTypes"`
	// Optional catalog hints for pricing workflows. Not used for model-scope gating.
	AvailabilityZones []string `yaml:"availabilityZones"`
}

// AutoscalingConfig configures ASG-based capacity management for Cluster Autoscaler
// and EKS Managed Nodegroup integrations.
//
// Per integration_strategy.md Section 4: Both CA and MNG rely on Auto Scaling Groups,
// so they share the same "Twin ASG" swap workflow (Scale-Wait-Drain).
type AutoscalingConfig struct {
	// Enabled enables ASG-based capacity management (for CA and MNG nodes).
	// When true, SpotVortex will manage Twin ASG pairs for spot/OD swaps.
	Enabled bool `yaml:"enabled"`

	// DiscoveryTags defines the ASG tag keys used to discover twin ASG pairs.
	DiscoveryTags ASGDiscoveryTags `yaml:"discoveryTags"`

	// NodeReadyTimeoutSeconds is how long to wait for a new node to become Ready
	// after scaling up a twin ASG. Default: 300 (5 minutes).
	NodeReadyTimeoutSeconds int `yaml:"nodeReadyTimeoutSeconds"`

	// PollIntervalSeconds is how often to poll for new node readiness. Default: 10.
	PollIntervalSeconds int `yaml:"pollIntervalSeconds"`
}

// ASGDiscoveryTags defines the tag keys used to discover twin ASG pairs.
type ASGDiscoveryTags struct {
	// Pool is the tag key for workload pool name. Default: "spotvortex.io/pool".
	Pool string `yaml:"pool"`

	// CapacityType is the tag key for capacity type (spot/on-demand).
	// Default: "spotvortex.io/capacity-type".
	CapacityType string `yaml:"type"`
}

// NodeReadyTimeout returns the node ready timeout as a duration.
func (a *AutoscalingConfig) NodeReadyTimeout() time.Duration {
	if a.NodeReadyTimeoutSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(a.NodeReadyTimeoutSeconds) * time.Second
}

// PollInterval returns the poll interval as a duration.
func (a *AutoscalingConfig) PollInterval() time.Duration {
	if a.PollIntervalSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(a.PollIntervalSeconds) * time.Second
}

// GCPConfig configures GCP preemptible pricing.
type GCPConfig struct {
	ProjectID string `yaml:"projectId"`
	Region    string `yaml:"region"`
	// Optional catalog hints for pricing workflows. Not used for model-scope gating.
	MachineTypes []string `yaml:"machineTypes"`
}

// Load reads configuration from a YAML file.
// Returns an error if file is missing or invalid.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// Validate checks required fields and applies safe defaults for optional fields.
func (c *Config) Validate() error {
	// Controller validation
	if c.Controller.RiskThreshold <= 0 || c.Controller.RiskThreshold > 1 {
		return fmt.Errorf("controller.riskThreshold must be between 0 and 1")
	}
	if c.Controller.MaxDrainRatio <= 0 || c.Controller.MaxDrainRatio > 1 {
		return fmt.Errorf("controller.maxDrainRatio must be between 0 and 1")
	}
	if c.Controller.ReconcileIntervalSeconds < 10 {
		return fmt.Errorf("controller.reconcileIntervalSeconds must be >= 10")
	}
	if c.Controller.ConfidenceThreshold <= 0 || c.Controller.ConfidenceThreshold > 1 {
		return fmt.Errorf("controller.confidenceThreshold must be between 0 and 1")
	}

	// Inference validation
	if c.Inference.TFTModelPath == "" {
		return fmt.Errorf("inference.tftModelPath is required")
	}
	if c.Inference.RLModelPath == "" {
		return fmt.Errorf("inference.rlModelPath is required")
	}
	if c.Inference.ModelManifestPath == "" {
		return fmt.Errorf("inference.modelManifestPath is required")
	}

	// Prometheus validation
	if c.Prometheus.URL == "" {
		return fmt.Errorf("prometheus.url is required")
	}

	// AWS defaults/validation.
	// Region is only used for AWS price-provider fallback. Keep a safe default.
	if c.AWS.Region == "" {
		c.AWS.Region = "us-east-1"
	}

	// Autoscaling validation - apply defaults for optional fields
	if c.Autoscaling.Enabled {
		if c.Autoscaling.DiscoveryTags.Pool == "" {
			c.Autoscaling.DiscoveryTags.Pool = "spotvortex.io/pool"
		}
		if c.Autoscaling.DiscoveryTags.CapacityType == "" {
			c.Autoscaling.DiscoveryTags.CapacityType = "spotvortex.io/capacity-type"
		}
		if c.Autoscaling.NodeReadyTimeoutSeconds == 0 {
			c.Autoscaling.NodeReadyTimeoutSeconds = 300
		}
		if c.Autoscaling.PollIntervalSeconds == 0 {
			c.Autoscaling.PollIntervalSeconds = 10
		}
	}

	// Karpenter validation - apply defaults for optional fields
	if c.Karpenter.Enabled {
		if c.Karpenter.SpotNodePoolSuffix == "" {
			c.Karpenter.SpotNodePoolSuffix = "-spot"
		}
		if c.Karpenter.OnDemandNodePoolSuffix == "" {
			c.Karpenter.OnDemandNodePoolSuffix = "-od"
		}
		if c.Karpenter.SpotWeight == 0 {
			c.Karpenter.SpotWeight = 100
		}
		if c.Karpenter.OnDemandWeight == 0 {
			c.Karpenter.OnDemandWeight = 10
		}
		if c.Karpenter.WeightChangeCooldownSeconds == 0 {
			c.Karpenter.WeightChangeCooldownSeconds = 60 // Default 60 seconds
		}
		// RespectDisruptionBudgets defaults to true when Karpenter is enabled
		// (set via yaml tag default, but ensure it's true if not explicitly set to false)
	}

	return nil
}

// ReconcileInterval returns the reconcile interval as a duration.
func (c *ControllerConfig) ReconcileInterval() time.Duration {
	return time.Duration(c.ReconcileIntervalSeconds) * time.Second
}

// DrainGracePeriod returns the drain grace period as a duration.
func (c *ControllerConfig) DrainGracePeriod() time.Duration {
	return time.Duration(c.DrainGracePeriodSeconds) * time.Second
}

// PrometheusTimeout returns the Prometheus timeout as a duration.
func (c *PrometheusConfig) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}
