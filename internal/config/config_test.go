package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_AllowsEmptyAWSCatalogLists(t *testing.T) {
	cfg := &Config{
		Controller: ControllerConfig{
			RiskThreshold:            0.85,
			MaxDrainRatio:            0.10,
			ReconcileIntervalSeconds: 30,
			ConfidenceThreshold:      0.50,
			DrainGracePeriodSeconds:  60,
		},
		Inference: InferenceConfig{
			TFTModelPath:      "models/tft.onnx",
			RLModelPath:       "models/rl_policy.onnx",
			ModelManifestPath: "models/MODEL_MANIFEST.json",
			ExpectedCloud:     "aws",
		},
		Prometheus: PrometheusConfig{
			URL:            "http://prometheus:9090",
			TimeoutSeconds: 10,
		},
		AWS: AWSConfig{
			Region: "",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate should allow empty aws.instanceTypes/aws.availabilityZones: %v", err)
	}
	if cfg.AWS.Region != "us-east-1" {
		t.Fatalf("expected default AWS region us-east-1, got %q", cfg.AWS.Region)
	}
}

func TestLoad_AllowsConfigWithoutAWSCatalogLists(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	content := `
controller:
  riskThreshold: 0.85
  maxDrainRatio: 0.10
  reconcileIntervalSeconds: 30
  confidenceThreshold: 0.50
  drainGracePeriodSeconds: 60
inference:
  tftModelPath: "models/tft.onnx"
  rlModelPath: "models/rl_policy.onnx"
  modelManifestPath: "models/MODEL_MANIFEST.json"
  expectedCloud: "aws"
prometheus:
  url: "http://prometheus:9090"
  timeoutSeconds: 10
aws:
  region: ""
gcp:
  projectId: ""
  region: "us-central1"
  machineTypes: []
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load should succeed without aws.instanceTypes/aws.availabilityZones: %v", err)
	}
	if cfg.AWS.Region != "us-east-1" {
		t.Fatalf("expected default AWS region us-east-1, got %q", cfg.AWS.Region)
	}
}
