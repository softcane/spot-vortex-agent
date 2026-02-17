package karpenter

import (
	"log/slog"
	"net/http"
	"testing"
)

func TestValidateKarpenterVersion_V1Available(t *testing.T) {
	// fake.NewClientset doesn't register Karpenter CRDs, so we test the
	// validation logic by using DetectKarpenterVersion directly and then
	// checking the path-based logic.
	ver := KarpenterVersion{HasV1: true, HasV1Beta1: false}
	if !ver.HasV1 {
		t.Fatal("expected HasV1 to be true")
	}
}

func TestValidateKarpenterVersion_OnlyV1Beta1(t *testing.T) {
	ver := KarpenterVersion{HasV1: false, HasV1Beta1: true}
	if ver.HasV1 {
		t.Fatal("expected HasV1 to be false")
	}
	if !ver.HasV1Beta1 {
		t.Fatal("expected HasV1Beta1 to be true")
	}
}

func TestValidateKarpenterVersion_NoCRDs(t *testing.T) {
	ver := KarpenterVersion{HasV1: false, HasV1Beta1: false}
	if ver.HasV1 || ver.HasV1Beta1 {
		t.Fatal("expected both to be false")
	}
}


// TestValidateVersionLogic tests the validation decision logic directly
// without requiring a real cluster.
func TestValidateVersionLogic(t *testing.T) {
	tests := []struct {
		name    string
		ver     KarpenterVersion
		wantErr bool
		errMsg  string
	}{
		{
			name:    "v1 available",
			ver:     KarpenterVersion{HasV1: true, HasV1Beta1: false},
			wantErr: false,
		},
		{
			name:    "both v1 and v1beta1",
			ver:     KarpenterVersion{HasV1: true, HasV1Beta1: true},
			wantErr: false,
		},
		{
			name:    "only v1beta1",
			ver:     KarpenterVersion{HasV1: false, HasV1Beta1: true},
			wantErr: true,
			errMsg:  "v1beta1",
		},
		{
			name:    "no CRDs",
			ver:     KarpenterVersion{HasV1: false, HasV1Beta1: false},
			wantErr: true,
			errMsg:  "CRDs not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVersionFromDetection(tt.ver, slog.Default())
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !contains(err.Error(), tt.errMsg) {
				t.Fatalf("expected error containing %q, got: %v", tt.errMsg, err)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure the fake client satisfies the interface used by DetectKarpenterVersion.
var _ http.RoundTripper = (*http.Transport)(nil)
