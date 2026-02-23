package inference

import (
	"errors"
	"testing"
)

func TestAsRLFallbackError(t *testing.T) {
	root := errors.New("rl model failed")
	err := &RLFallbackError{
		CapacityScore: 0.72,
		RuntimeScore:  0.19,
		Cause:         root,
	}

	got, ok := AsRLFallbackError(err)
	if !ok {
		t.Fatal("expected AsRLFallbackError to match")
	}
	if got.CapacityScore != 0.72 || got.RuntimeScore != 0.19 {
		t.Fatalf("unexpected scores: capacity=%v runtime=%v", got.CapacityScore, got.RuntimeScore)
	}
	if !errors.Is(err, root) {
		t.Fatal("expected fallback error to unwrap to root cause")
	}
}

func TestAsRLFallbackError_NoMatch(t *testing.T) {
	if _, ok := AsRLFallbackError(errors.New("plain error")); ok {
		t.Fatal("expected no fallback match for plain error")
	}
}
