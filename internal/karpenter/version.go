package karpenter

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/client-go/kubernetes"
)

// KarpenterVersion holds the detected Karpenter API version availability.
type KarpenterVersion struct {
	HasV1      bool // Karpenter v1.0+ (karpenter.sh/v1)
	HasV1Beta1 bool // Karpenter pre-v1.0 (karpenter.sh/v1beta1)
}

// DetectKarpenterVersion probes the API server for Karpenter CRD versions.
func DetectKarpenterVersion(ctx context.Context, k8s kubernetes.Interface) KarpenterVersion {
	var ver KarpenterVersion

	// Check for v1 (Karpenter v1.0+)
	_, err := k8s.Discovery().RESTClient().
		Get().
		AbsPath("/apis/karpenter.sh/v1/nodepools").
		DoRaw(ctx)
	ver.HasV1 = err == nil

	// Check for v1beta1 (older Karpenter)
	_, err = k8s.Discovery().RESTClient().
		Get().
		AbsPath("/apis/karpenter.sh/v1beta1/nodepools").
		DoRaw(ctx)
	ver.HasV1Beta1 = err == nil

	return ver
}

// ValidateKarpenterVersion checks that the cluster runs a supported Karpenter version.
// Returns nil if v1 is available. Returns an error if only v1beta1 or nothing is found.
func ValidateKarpenterVersion(ctx context.Context, k8s kubernetes.Interface, logger *slog.Logger) error {
	ver := DetectKarpenterVersion(ctx, k8s)
	return validateVersionFromDetection(ver, logger)
}

// validateVersionFromDetection applies version validation logic on a pre-detected version.
// Exported for testing without a real cluster.
func validateVersionFromDetection(ver KarpenterVersion, logger *slog.Logger) error {
	if ver.HasV1 {
		logger.Info("Karpenter v1 API detected (supported)")
		return nil
	}

	if ver.HasV1Beta1 {
		return fmt.Errorf("karpenter v1beta1 detected but v1 is required; " +
			"please upgrade Karpenter to v1.0+: https://karpenter.sh/docs/upgrading/v1-migration/")
	}

	return fmt.Errorf("karpenter CRDs not found: neither karpenter.sh/v1 nor karpenter.sh/v1beta1 are available; " +
		"install Karpenter or disable karpenter.enabled in config")
}

