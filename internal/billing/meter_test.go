package billing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMeter_E2E(t *testing.T) {
	// Setup mock server
	var receivedReq *http.Request
	var receivedBody SavingsEvent

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedReq = r
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("failed to decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Config
	cfg := MeterConfig{
		Endpoint: ts.URL,
		Enabled:  true,
		Logger:   slog.Default(),
	}
	meter := NewMeter(cfg)

	// Act 1: Track Start
	meter.TrackNodeStart("node-1", "m5.large", "us-east-1", "us-east-1a", 0.1, 0.2) // Savings = $0.10/hr

	// Manipulate time to simulate 1 hour passing
	meter.mu.Lock()
	tracker := meter.activeNodes["node-1"]
	tracker.StartTime = time.Now().Add(-60 * time.Minute)
	meter.activeNodes["node-1"] = tracker
	meter.mu.Unlock()

	// Verify GetActiveSavings
	active := meter.GetActiveSavings()
	// Should be approx 0.1
	if active < 0.09 || active > 0.11 {
		t.Errorf("expected active savings ~0.1, got %f", active)
	}

	// Act 2: Track End (triggers report)
	err := meter.TrackNodeEnd(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("TrackNodeEnd failed: %v", err)
	}

	// Verify request
	if receivedReq == nil {
		t.Fatal("server did not receive request")
	}
	if receivedReq.Header.Get("X-SpotVortex-Version") != "1.1.0" {
		t.Errorf("missing version header")
	}
	if receivedBody.NodeID != "node-1" {
		t.Errorf("expected node-1, got %s", receivedBody.NodeID)
	}
	// Uptime 60 mins -> 0.1 savings
	if receivedBody.UptimeMinutes < 60 {
		t.Errorf("expected >= 60 uptime minutes, got %d", receivedBody.UptimeMinutes)
	}
	if receivedBody.Savings < 0.09 {
		t.Errorf("expected savings ~0.1, got %f", receivedBody.Savings)
	}
}

func TestMeter_DryRun(t *testing.T) {
	// Config
	cfg := MeterConfig{
		Endpoint: "http://localhost:12345", // Should not call
		Enabled:  true,
		DryRun:   true,
		Logger:   slog.Default(),
	}
	meter := NewMeter(cfg)

	meter.TrackNodeStart("dry-node", "t3.micro", "us-west-2", "us-west-2a", 0.01, 0.02)

	// End
	err := meter.TrackNodeEnd(context.Background(), "dry-node")
	if err != nil {
		t.Errorf("TrackNodeEnd failed in dry run: %v", err)
	}
	// Pass if no panic and no networking error (invalid port would fail if real call made)
}

func TestMeter_Disabled(t *testing.T) {
	cfg := MeterConfig{
		Enabled: false,
		Logger:  slog.Default(),
	}
	meter := NewMeter(cfg)

	meter.TrackNodeStart("disabled-node", "t3.micro", "us-west-2", "us-west-2a", 0.01, 0.02)
	err := meter.TrackNodeEnd(context.Background(), "disabled-node")
	if err != nil {
		t.Errorf("TrackNodeEnd failed when disabled: %v", err)
	}
}

func TestMeter_UntrackedNode(t *testing.T) {
	meter := NewMeter(MeterConfig{Logger: slog.Default(), Enabled: true})
	err := meter.TrackNodeEnd(context.Background(), "ghost-node")
	if err != nil {
		t.Errorf("Untracked node should not error: %v", err)
	}
}
