package inference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelContractFromManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "MODEL_MANIFEST.json")

	manifest := map[string]any{
		"cloud":                       "aws",
		"supported_instance_families": []string{"c6i", "m6a"},
		"artifacts": map[string]any{
			"tft.onnx": map[string]any{"path": "tft.onnx", "sha256": "abc"},
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, payload, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	contract, err := LoadModelContract(manifestPath)
	if err != nil {
		t.Fatalf("LoadModelContract failed: %v", err)
	}
	if contract == nil {
		t.Fatal("expected non-nil contract")
	}
	if contract.Cloud != "aws" {
		t.Fatalf("expected cloud aws, got %q", contract.Cloud)
	}
	if len(contract.SupportedInstanceFamilies) != 2 {
		t.Fatalf("expected 2 families, got %d", len(contract.SupportedInstanceFamilies))
	}
	if contract.ArtifactChecksums["tft.onnx"] != "abc" {
		t.Fatalf("expected artifact checksum to be loaded")
	}
}

func TestLoadModelContractEnvOverrides(t *testing.T) {
	t.Setenv("SPOTVORTEX_MODEL_CLOUD", "gcp")
	t.Setenv("SPOTVORTEX_SUPPORTED_INSTANCE_FAMILIES", "n2, c3")

	contract, err := LoadModelContract("")
	if err != nil {
		t.Fatalf("LoadModelContract failed: %v", err)
	}
	if contract == nil {
		t.Fatal("expected contract from env overrides")
	}
	if contract.Cloud != "gcp" {
		t.Fatalf("expected env cloud gcp, got %q", contract.Cloud)
	}
	if len(contract.SupportedInstanceFamilies) != 2 {
		t.Fatalf("expected 2 env families, got %d", len(contract.SupportedInstanceFamilies))
	}
}

func TestVerifyManifestArtifacts(t *testing.T) {
	dir := t.TempDir()
	tft := filepath.Join(dir, "tft.onnx")
	tftData := tft + ".data"
	rl := filepath.Join(dir, "rl_policy.onnx")
	rlData := rl + ".data"

	files := map[string]string{
		tft:     "tft-model",
		tftData: "tft-data",
		rl:      "rl-model",
		rlData:  "rl-data",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	tftHash, err := hashFileSHA256(tft)
	if err != nil {
		t.Fatalf("hash tft: %v", err)
	}
	tftDataHash, err := hashFileSHA256(tftData)
	if err != nil {
		t.Fatalf("hash tft data: %v", err)
	}
	rlHash, err := hashFileSHA256(rl)
	if err != nil {
		t.Fatalf("hash rl: %v", err)
	}
	rlDataHash, err := hashFileSHA256(rlData)
	if err != nil {
		t.Fatalf("hash rl data: %v", err)
	}

	manifestPath := filepath.Join(dir, "MODEL_MANIFEST.json")
	manifest := map[string]any{
		"cloud":                       "aws",
		"supported_instance_families": []string{"c6i"},
		"artifacts": map[string]any{
			"tft.onnx":            map[string]any{"path": "tft.onnx", "sha256": tftHash},
			"tft.onnx.data":       map[string]any{"path": "tft.onnx.data", "sha256": tftDataHash},
			"rl_policy.onnx":      map[string]any{"path": "rl_policy.onnx", "sha256": rlHash},
			"rl_policy.onnx.data": map[string]any{"path": "rl_policy.onnx.data", "sha256": rlDataHash},
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, payload, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if err := VerifyManifestArtifacts(manifestPath, tft, rl); err != nil {
		t.Fatalf("expected verification success, got: %v", err)
	}
}

func TestVerifyManifestArtifactsMismatch(t *testing.T) {
	dir := t.TempDir()
	tft := filepath.Join(dir, "tft.onnx")
	rl := filepath.Join(dir, "rl_policy.onnx")
	if err := os.WriteFile(tft, []byte("tft"), 0o644); err != nil {
		t.Fatalf("write tft: %v", err)
	}
	if err := os.WriteFile(rl, []byte("rl"), 0o644); err != nil {
		t.Fatalf("write rl: %v", err)
	}

	manifestPath := filepath.Join(dir, "MODEL_MANIFEST.json")
	manifest := map[string]any{
		"artifacts": map[string]any{
			"tft.onnx":       map[string]any{"path": "tft.onnx", "sha256": strings.Repeat("0", 64)},
			"rl_policy.onnx": map[string]any{"path": "rl_policy.onnx", "sha256": strings.Repeat("0", 64)},
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, payload, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	err = VerifyManifestArtifacts(manifestPath, tft, rl)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}
