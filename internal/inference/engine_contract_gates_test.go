package inference

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

func TestNewInferenceEngine_FailsOnManifestChecksumMismatch(t *testing.T) {
	modelsDir := filepath.Clean(filepath.Join("..", "..", "models"))
	required := []string{
		filepath.Join(modelsDir, "tft.onnx"),
		filepath.Join(modelsDir, "rl_policy.onnx"),
		filepath.Join(modelsDir, "MODEL_MANIFEST.json"),
	}
	for _, path := range required {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("required model fixture missing (%s): %v", path, err)
		}
	}

	dir := t.TempDir()
	tft := filepath.Join(dir, "tft.onnx")
	rl := filepath.Join(dir, "rl_policy.onnx")
	manifestPath := filepath.Join(dir, "MODEL_MANIFEST.json")

	copyFile(t, filepath.Join(modelsDir, "tft.onnx"), tft)
	copyFile(t, filepath.Join(modelsDir, "rl_policy.onnx"), rl)
	copyFile(t, filepath.Join(modelsDir, "MODEL_MANIFEST.json"), manifestPath)
	copyONNXDataFiles(t, modelsDir, dir)

	var manifest map[string]any
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	artifacts, ok := manifest["artifacts"].(map[string]any)
	if !ok {
		t.Fatalf("manifest artifacts has unexpected type %T", manifest["artifacts"])
	}
	rlArtifact, ok := artifacts["rl_policy.onnx"].(map[string]any)
	if !ok {
		t.Fatalf("manifest rl_policy.onnx has unexpected type %T", artifacts["rl_policy.onnx"])
	}
	rlArtifact["sha256"] = strings.Repeat("0", 64)

	updated, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, updated, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	_, err = NewInferenceEngine(EngineConfig{
		TFTModelPath:         tft,
		RLModelPath:          rl,
		ModelManifestPath:    manifestPath,
		RequireModelContract: true,
		RequireRuntimeHead:   false,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatal("expected NewInferenceEngine to fail on manifest checksum mismatch")
	}
	if !strings.Contains(err.Error(), "manifest artifact verification failed") {
		t.Fatalf("expected manifest verification error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch detail, got: %v", err)
	}
}

func TestValidateTFTOutputContract_FailsWhenRuntimeHeadRequiredMissing(t *testing.T) {
	ensureORTTensorsAvailable(t)

	capacity, err := ort.NewTensor(ort.NewShape(1), []float32{0.25})
	if err != nil {
		t.Fatalf("create capacity tensor: %v", err)
	}
	defer capacity.Destroy()

	err = validateTFTOutputContract(map[string]*ort.Tensor[float32]{
		"capacity_score": capacity,
	}, true)
	if err == nil {
		t.Fatal("expected runtime-head-required contract error")
	}
	if !strings.Contains(err.Error(), "runtime head is required but missing") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRLQValuesOutputContract_FailsOnWrongActionShape(t *testing.T) {
	ensureORTTensorsAvailable(t)

	q, err := ort.NewTensor(ort.NewShape(1, 5), []float32{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatalf("create q_values tensor: %v", err)
	}
	defer q.Destroy()

	err = validateRLQValuesOutputContract(map[string]*ort.Tensor[float32]{
		"q_values": q,
	})
	if err == nil {
		t.Fatal("expected RL q_values shape contract error")
	}
	if !strings.Contains(err.Error(), "last dimension=6") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func ensureORTTensorsAvailable(t *testing.T) {
	t.Helper()
	SetSharedLibraryPath()
	_ = ort.InitializeEnvironment()
	tensor, err := ort.NewTensor(ort.NewShape(1), []float32{1})
	if err != nil {
		t.Skipf("ONNX Runtime tensor support unavailable: %v", err)
	}
	tensor.Destroy()
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func copyONNXDataFiles(t *testing.T, srcDir, dstDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(srcDir, "*.onnx.data"))
	if err != nil {
		t.Fatalf("glob onnx data files: %v", err)
	}
	for _, src := range matches {
		copyFile(t, src, filepath.Join(dstDir, filepath.Base(src)))
	}
}
