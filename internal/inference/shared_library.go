package inference

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	ort "github.com/yalue/onnxruntime_go"
)

func SetSharedLibraryPath() {
	paths := []string{}
	if env := os.Getenv("ORT_SHARED_LIBRARY_PATH"); env != "" {
		paths = appendSharedLibraryCandidates(paths, env)
	}
	if env := os.Getenv("SPOTVORTEX_ONNXRUNTIME_PATH"); env != "" {
		paths = appendSharedLibraryCandidates(paths, env)
	}
	paths = appendVenvONNXRuntimeCandidates(paths)
	paths = append(paths, []string{
		"/opt/homebrew/lib/libonnxruntime.dylib",
		"/usr/local/lib/libonnxruntime.dylib",
		"/usr/lib/libonnxruntime.so",
	}...)

	seen := map[string]struct{}{}
	for _, p := range paths {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if _, err := os.Stat(p); err == nil {
			ort.SetSharedLibraryPath(p)
			return
		}
	}

	ort.SetSharedLibraryPath("onnxruntime")
}

func appendSharedLibraryCandidates(paths []string, rawPath string) []string {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return paths
	}

	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return append(paths, path)
	}

	startLen := len(paths)
	patterns := []string{
		filepath.Join(path, "libonnxruntime.so"),
		filepath.Join(path, "libonnxruntime.so.*"),
		filepath.Join(path, "libonnxruntime.dylib"),
		filepath.Join(path, "libonnxruntime*.dylib"),
	}
	for _, pattern := range patterns {
		matches, globErr := filepath.Glob(pattern)
		if globErr != nil {
			continue
		}
		sort.Strings(matches)
		paths = append(paths, matches...)
	}

	if len(paths) == startLen {
		paths = append(paths, path)
	}
	return paths
}

func appendVenvONNXRuntimeCandidates(paths []string) []string {
	basePatterns := []string{
		".venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		".venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"vortex/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"vortex/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
		"spot-vortex/tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.dylib",
		"spot-vortex/tests/e2e/.venv/lib/python*/site-packages/onnxruntime/capi/libonnxruntime*.so*",
	}
	prefixes := []string{
		".",
		"..",
		"../..",
		"../../..",
		"../../../..",
	}
	for _, prefix := range prefixes {
		for _, base := range basePatterns {
			pattern := filepath.Join(prefix, base)
			matches, err := filepath.Glob(pattern)
			if err != nil {
				continue
			}
			paths = append(paths, matches...)
		}
	}
	return paths
}
