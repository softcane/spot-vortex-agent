package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// MockAPI implements v1.API for testing
type MockAPI struct {
	v1.API
	QueryResult model.Value
	QueryErr    error
	Warnings    v1.Warnings
}

func (m *MockAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	return m.QueryResult, m.Warnings, m.QueryErr
}

func TestNewClient(t *testing.T) {
	// ... (TestNewClient remains same)
	tests := []struct {
		name    string
		cfg     ClientConfig
		wantErr bool
	}{
		{
			name: "valid config",
			cfg: ClientConfig{
				PrometheusURL: "http://localhost:9090",
				Logger:        slog.Default(),
			},
			wantErr: false,
		},
		{
			name: "missing url and api",
			cfg: ClientConfig{
				Logger: slog.Default(),
			},
			wantErr: true,
		},
		{
			name: "provided api",
			cfg: ClientConfig{
				Logger: slog.Default(),
				API:    &MockAPI{},
			},
			wantErr: false,
		},
	}
	// ... loop
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

type QueryFunc func(query string) (model.Value, error)

type SmartMockAPI struct {
	v1.API
	QueryFn QueryFunc
}

func (m *SmartMockAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	val, err := m.QueryFn(query)
	return val, nil, err
}

func TestGetNodeMetrics_Success(t *testing.T) {
	mockAPI := &SmartMockAPI{
		QueryFn: func(query string) (model.Value, error) {
			if strings.Contains(query, "node_cpu_seconds_total") {
				// CPU Query
				return model.Vector{
					{Metric: model.Metric{"node": "node-1"}, Value: 25.5},
				}, nil
			} else if strings.Contains(query, "node_memory_MemTotal_bytes") {
				// Memory Query
				return model.Vector{
					{Metric: model.Metric{"node": "node-1"}, Value: 40.2},
				}, nil
			}
			return nil, fmt.Errorf("unexpected query: %s", query)
		},
	}

	client := &Client{api: mockAPI, logger: slog.Default()}
	metrics, err := client.GetNodeMetrics(context.Background())

	if err != nil {
		t.Fatalf("GetNodeMetrics failed: %v", err)
	}

	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}

	if metrics[0].NodeID != "node-1" {
		t.Errorf("expected node-1, got %s", metrics[0].NodeID)
	}
	if metrics[0].CPUUsagePercent != 25.5 {
		t.Errorf("expected 25.5 CPU, got %f", metrics[0].CPUUsagePercent)
	}
	if metrics[0].MemoryUsagePercent != 40.2 {
		t.Errorf("expected 40.2 Mem, got %f", metrics[0].MemoryUsagePercent)
	}
}

func TestGetClusterUtilization(t *testing.T) {
	tests := []struct {
		name    string
		mockVal model.Value
		mockErr error
		want    float64
		wantErr bool
	}{
		{
			name: "vector result",
			mockVal: model.Vector{
				{Value: 45.0}, // 45% -> 0.45
			},
			want: 0.45,
		},
		{
			name: "scalar result",
			mockVal: &model.Scalar{
				Value: 60.0, // 60% -> 0.60
			},
			want: 0.60,
		},
		{
			name:    "query error",
			mockErr: fmt.Errorf("prom error"),
			wantErr: true,
		},
		{
			name:    "empty result",
			mockVal: model.Vector{},
			want:    0.5, // Default
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAPI := &MockAPI{
				QueryResult: tt.mockVal,
				QueryErr:    tt.mockErr,
			}
			client := &Client{api: mockAPI, logger: slog.Default()}

			got, err := client.GetClusterUtilization(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("GetClusterUtilization() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("GetClusterUtilization() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPoolUtilization(t *testing.T) {
	mockAPI := &MockAPI{
		QueryResult: model.Vector{
			{
				Metric: model.Metric{
					"node_kubernetes_io_instance_type": "m5.large",
					"topology_kubernetes_io_zone":      "us-east-1a",
				},
				Value: 75.0, // 0.75
			},
		},
	}
	client := &Client{api: mockAPI, logger: slog.Default()}

	utils, err := client.GetPoolUtilization(context.Background())
	if err != nil {
		t.Fatalf("GetPoolUtilization failed: %v", err)
	}

	val, ok := utils["m5.large:us-east-1a"]
	if !ok {
		t.Fatal("expected pool key not found")
	}
	if val != 0.75 {
		t.Errorf("expected 0.75, got %f", val)
	}
}
