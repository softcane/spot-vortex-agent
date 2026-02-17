package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ModelContract captures runtime scope restrictions for a model bundle.
//
// Keep this intentionally minimal and permissive so Phase-1 manual bundles can
// be treated as opaque artifacts while still enforcing cloud/family boundaries.
type ModelContract struct {
	Cloud                     string
	SupportedInstanceFamilies []string
	ArtifactChecksums         map[string]string
}

type manifestArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type modelManifest struct {
	Cloud                     string                      `json:"cloud"`
	SupportedInstanceFamilies []string                    `json:"supported_instance_families"`
	Artifacts                 map[string]manifestArtifact `json:"artifacts"`
	Models                    map[string]manifestArtifact `json:"models"`
	ModelScope                struct {
		Cloud                     string   `json:"cloud"`
		SupportedInstanceFamilies []string `json:"supported_instance_families"`
	} `json:"model_scope"`
}

// LoadModelContract loads an optional model contract from manifest and env.
//
// Resolution order:
// 1. MODEL_MANIFEST.json (if present)
// 2. Env overrides:
//   - SPOTVORTEX_MODEL_CLOUD
//   - SPOTVORTEX_SUPPORTED_INSTANCE_FAMILIES (comma-separated)
//
// Returns (nil, nil) when no contract is available.
func LoadModelContract(manifestPath string) (*ModelContract, error) {
	contract := &ModelContract{ArtifactChecksums: make(map[string]string)}
	found := false

	if strings.TrimSpace(manifestPath) != "" {
		payload, err := os.ReadFile(manifestPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
			}
		} else {
			var manifest modelManifest
			if err := json.Unmarshal(payload, &manifest); err != nil {
				return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
			}

			cloud := strings.TrimSpace(strings.ToLower(manifest.Cloud))
			if cloud == "" {
				cloud = strings.TrimSpace(strings.ToLower(manifest.ModelScope.Cloud))
			}
			contract.Cloud = cloud

			families := append([]string{}, manifest.SupportedInstanceFamilies...)
			if len(families) == 0 {
				families = append(families, manifest.ModelScope.SupportedInstanceFamilies...)
			}
			contract.SupportedInstanceFamilies = normalizeFamilies(families)

			collectArtifactChecksums(contract.ArtifactChecksums, manifest.Artifacts)
			collectArtifactChecksums(contract.ArtifactChecksums, manifest.Models)
			found = true
		}
	}

	if envCloud := strings.TrimSpace(strings.ToLower(os.Getenv("SPOTVORTEX_MODEL_CLOUD"))); envCloud != "" {
		contract.Cloud = envCloud
		found = true
	}
	if envFamilies := strings.TrimSpace(os.Getenv("SPOTVORTEX_SUPPORTED_INSTANCE_FAMILIES")); envFamilies != "" {
		parts := strings.Split(envFamilies, ",")
		contract.SupportedInstanceFamilies = normalizeFamilies(parts)
		found = true
	}

	if !found {
		return nil, nil
	}
	return contract, nil
}

func collectArtifactChecksums(dst map[string]string, artifacts map[string]manifestArtifact) {
	for key, art := range artifacts {
		path := strings.TrimSpace(art.Path)
		if path == "" {
			path = key
		}
		sum := strings.TrimSpace(strings.ToLower(art.SHA256))
		if path == "" || sum == "" {
			continue
		}
		dst[normalizeArtifactPath(path)] = sum
	}
}

// VerifyManifestArtifacts validates artifact checksums for required model files.
//
// Manifest keys can use either full relative paths (for example, "models/tft.onnx")
// or basenames (for example, "tft.onnx").
func VerifyManifestArtifacts(manifestPath string, requiredPaths ...string) error {
	manifestPath = strings.TrimSpace(manifestPath)
	if manifestPath == "" {
		return fmt.Errorf("manifest path is required")
	}

	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}

	var manifest modelManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}

	expected := make(map[string]string)
	collectArtifactChecksums(expected, manifest.Artifacts)
	collectArtifactChecksums(expected, manifest.Models)
	if len(expected) == 0 {
		return fmt.Errorf("manifest %s has no artifact checksums", manifestPath)
	}

	seen := make(map[string]struct{})
	artifacts := make([]string, 0, len(requiredPaths)*2)
	for _, raw := range requiredPaths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			artifacts = append(artifacts, path)
		}
		if _, err := os.Stat(path + ".data"); err == nil {
			sidecar := path + ".data"
			if _, ok := seen[sidecar]; !ok {
				seen[sidecar] = struct{}{}
				artifacts = append(artifacts, sidecar)
			}
		}
	}

	for _, artifactPath := range artifacts {
		key := manifestChecksumKey(expected, artifactPath)
		if key == "" {
			return fmt.Errorf("manifest %s missing checksum entry for %s", manifestPath, artifactPath)
		}
		actual, err := hashFileSHA256(artifactPath)
		if err != nil {
			return fmt.Errorf("hash artifact %s: %w", artifactPath, err)
		}
		expectedSum := expected[key]
		if !strings.EqualFold(actual, expectedSum) {
			return fmt.Errorf("checksum mismatch for %s: expected=%s actual=%s", artifactPath, expectedSum, actual)
		}
	}

	return nil
}

func manifestChecksumKey(expected map[string]string, artifactPath string) string {
	keys := []string{
		normalizeArtifactPath(artifactPath),
		normalizeArtifactPath(filepath.Base(artifactPath)),
	}
	for _, key := range keys {
		if _, ok := expected[key]; ok {
			return key
		}
	}
	return ""
}

func hashFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func normalizeFamilies(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		value := strings.TrimSpace(strings.ToLower(raw))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func defaultManifestPath(tftModelPath string) string {
	if strings.TrimSpace(tftModelPath) == "" {
		return "models/MODEL_MANIFEST.json"
	}
	return filepath.Join(filepath.Dir(tftModelPath), "MODEL_MANIFEST.json")
}

func normalizeArtifactPath(path string) string {
	path = strings.TrimSpace(strings.ToLower(path))
	if path == "" {
		return ""
	}
	path = filepath.ToSlash(path)
	path = strings.TrimPrefix(path, "./")
	return path
}

// InstanceFamilyLabel returns a low-cardinality instance family label.
func InstanceFamilyLabel(instanceType string) string {
	instanceType = strings.TrimSpace(strings.ToLower(instanceType))
	if instanceType == "" || instanceType == "unknown" {
		return "unknown"
	}
	family, _, _ := strings.Cut(instanceType, ".")
	if family == "" {
		return "unknown"
	}
	return family
}

// SupportsInstanceType checks whether instanceType is in contract scope.
func (m *ModelContract) SupportsInstanceType(instanceType string) (bool, string) {
	if m == nil || len(m.SupportedInstanceFamilies) == 0 {
		return true, ""
	}

	instanceType = strings.TrimSpace(strings.ToLower(instanceType))
	if instanceType == "" || instanceType == "unknown" {
		return false, "instance type is unknown and model scope is restricted"
	}
	family := InstanceFamilyLabel(instanceType)

	for _, token := range m.SupportedInstanceFamilies {
		// Exact family token: "c6i"
		if !strings.Contains(token, ".") && !strings.HasSuffix(token, "*") {
			if token == family {
				return true, ""
			}
			continue
		}

		// Family wildcard: "c6i.*" or "c6i*"
		if strings.HasSuffix(token, ".*") || strings.HasSuffix(token, "*") {
			prefix := strings.TrimSuffix(strings.TrimSuffix(token, ".*"), "*")
			if strings.HasPrefix(instanceType, prefix+".") || family == prefix {
				return true, ""
			}
			continue
		}

		// Exact instance type: "c6i.2xlarge"
		if token == instanceType {
			return true, ""
		}
	}

	return false, fmt.Sprintf("instance family %q not supported by current model", family)
}
