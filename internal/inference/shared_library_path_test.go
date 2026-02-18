package inference

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendSharedLibraryCandidates_FilePath(t *testing.T) {
	tmpDir := t.TempDir()
	libPath := filepath.Join(tmpDir, "libonnxruntime.so")
	if err := os.WriteFile(libPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fake lib: %v", err)
	}

	got := appendSharedLibraryCandidates(nil, libPath)
	if len(got) != 1 || got[0] != libPath {
		t.Fatalf("unexpected candidates: %#v", got)
	}
}

func TestAppendSharedLibraryCandidates_DirectoryPath(t *testing.T) {
	tmpDir := t.TempDir()
	libV := filepath.Join(tmpDir, "libonnxruntime.so.1.23.2")
	lib := filepath.Join(tmpDir, "libonnxruntime.so")
	if err := os.WriteFile(libV, []byte("fake-v"), 0o644); err != nil {
		t.Fatalf("write fake versioned lib: %v", err)
	}
	if err := os.WriteFile(lib, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fake unversioned lib: %v", err)
	}

	got := appendSharedLibraryCandidates(nil, tmpDir)
	if len(got) < 2 {
		t.Fatalf("expected multiple candidates for directory path, got %#v", got)
	}
	if !containsString(got, libV) {
		t.Fatalf("expected versioned library candidate, got %#v", got)
	}
	if !containsString(got, lib) {
		t.Fatalf("expected library candidate, got %#v", got)
	}
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
