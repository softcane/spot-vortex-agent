package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func requireMonitoringSuite(t *testing.T) {
	t.Helper()
	if os.Getenv(e2eSuiteEnv) != "monitoring" {
		t.Skipf("set %s=monitoring to run this suite", e2eSuiteEnv)
	}
}

type grafanaSearchEntry struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
}

type grafanaDashboardResponse struct {
	Dashboard grafanaDashboard `json:"dashboard"`
}

type grafanaDashboard struct {
	UID    string         `json:"uid"`
	Title  string         `json:"title"`
	Panels []grafanaPanel `json:"panels"`
}

type grafanaPanel struct {
	Title   string          `json:"title"`
	Type    string          `json:"type"`
	Targets []grafanaTarget `json:"targets"`
}

type grafanaTarget struct {
	Expr string `json:"expr"`
}

type prometheusQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string                 `json:"resultType"`
		Result     []prometheusQueryPoint `json:"result"`
	} `json:"data"`
}

type prometheusQueryPoint struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

func TestMonitoring_DashboardsProvisioned(t *testing.T) {
	requireMonitoringSuite(t)

	grafanaURL := startPortForward(t, "monitoring", "grafana", 3000)
	search := queryGrafanaSearch(t, grafanaURL)

	expected := map[string]string{
		"spotvortex-dryrun":     "SpotVortex Dry-Run Value Preview",
		"spotvortex-ops-shadow": "SpotVortex Ops Shadow",
	}
	for uid, title := range expected {
		if !grafanaSearchContains(search, uid, title) {
			t.Fatalf("Grafana search missing dashboard uid=%q title=%q", uid, title)
		}
		dashboard := queryGrafanaDashboard(t, grafanaURL, uid)
		if dashboard.Title != title {
			t.Fatalf("dashboard uid=%q title=%q, want %q", uid, dashboard.Title, title)
		}
		if len(dashboard.Panels) == 0 {
			t.Fatalf("dashboard uid=%q has no panels", uid)
		}
		assertDashboardIsClean(t, dashboard)
	}
}

func TestMonitoring_RuntimeMetricsReachPrometheus(t *testing.T) {
	requireMonitoringSuite(t)

	promURL := startPortForward(t, "monitoring", "prometheus", 9090)

	queries := []struct {
		name  string
		query string
	}{
		{
			name:  "prometheus scraping Helm metrics service",
			query: `min(up{job="spotvortex-agent",namespace="spotvortex",service="spotvortex-metrics"})`,
		},
		{
			name:  "dry-run savings preview is populated",
			query: `sum(spotvortex_potential_savings_pool_total_usd)`,
		},
		{
			name:  "dry-run node opportunities are populated",
			query: `sum(spotvortex_nodes_optimizable)`,
		},
		{
			name:  "dry-run spot ratio series exist",
			query: `count(spotvortex_spot_ratio_current)`,
		},
		{
			name:  "dry-run recommendations exist",
			query: `count(spotvortex_recommended_action)`,
		},
		{
			name:  "price signals exist",
			query: `count(spotvortex_spot_price_usd)`,
		},
		{
			name:  "deterministic decisions were emitted",
			query: `sum(spotvortex_decision_source_total)`,
		},
		{
			name:  "deterministic decision reasons were emitted",
			query: `sum(spotvortex_deterministic_decision_reason_total)`,
		},
		{
			name:  "capacity score exists",
			query: `count(spotvortex_capacity_score)`,
		},
		{
			name:  "runtime score exists",
			query: `count(spotvortex_runtime_score)`,
		},
		{
			name:  "workload cap exists",
			query: `count(spotvortex_workload_cap)`,
		},
		{
			name:  "workload OOD flag exists",
			query: `count(spotvortex_workload_ood)`,
		},
		{
			name:  "RL shadow recommendations were emitted",
			query: `sum(spotvortex_shadow_action_recommended_total)`,
		},
		{
			name:  "RL shadow agreement was emitted",
			query: `sum(spotvortex_shadow_action_agreement_total)`,
		},
		{
			name:  "RL shadow projected delta exists",
			query: `count(spotvortex_shadow_projected_savings_delta_usd)`,
		},
		{
			name:  "inference latency histogram exists",
			query: `count(spotvortex_inference_latency_seconds_bucket)`,
		},
		{
			name:  "reconcile latency histogram exists",
			query: `count(spotvortex_reconcile_loop_duration_seconds_bucket)`,
		},
		{
			name:  "pending duration histogram exists",
			query: `count(spotvortex_pod_pending_duration_seconds_bucket)`,
		},
		{
			name:  "recovery time histogram exists",
			query: `count(spotvortex_recovery_time_seconds_bucket)`,
		},
	}

	for _, tc := range queries {
		value := waitForPrometheusValue(t, promURL, tc.query, 2*time.Minute)
		if value <= 0 {
			t.Fatalf("%s query=%q returned non-positive value %.3f", tc.name, tc.query, value)
		}
	}
}

func assertDashboardIsClean(t *testing.T, dashboard grafanaDashboard) {
	t.Helper()

	bannedFragments := []string{
		"placeholder",
		"not wired yet",
		"spotvortex_savings_usd_hourly",
		"spotvortex_market_volatility",
		"spotvortex_nodes_managed",
		"spotvortex_nodes_draining",
		"spotvortex_spot_ratio_target",
	}

	for _, panel := range dashboard.Panels {
		title := strings.ToLower(strings.TrimSpace(panel.Title))
		for _, banned := range bannedFragments {
			if strings.Contains(title, banned) {
				t.Fatalf("dashboard uid=%q contains stale panel title %q", dashboard.UID, panel.Title)
			}
		}
		for _, target := range panel.Targets {
			expr := strings.ToLower(target.Expr)
			for _, banned := range bannedFragments {
				if strings.Contains(expr, banned) {
					t.Fatalf("dashboard uid=%q contains stale query fragment %q in expr %q", dashboard.UID, banned, target.Expr)
				}
			}
		}
	}
}

func queryGrafanaSearch(t *testing.T, grafanaURL string) []grafanaSearchEntry {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, grafanaURL+"/api/search?query=SpotVortex", nil)
	if err != nil {
		t.Fatalf("build grafana search request: %v", err)
	}
	req.SetBasicAuth("admin", "admin")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("grafana search request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("grafana search status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result []grafanaSearchEntry
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode grafana search response: %v", err)
	}
	return result
}

func grafanaSearchContains(entries []grafanaSearchEntry, uid, title string) bool {
	for _, entry := range entries {
		if entry.UID == uid && entry.Title == title {
			return true
		}
	}
	return false
}

func queryGrafanaDashboard(t *testing.T, grafanaURL, uid string) grafanaDashboard {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, grafanaURL+"/api/dashboards/uid/"+uid, nil)
	if err != nil {
		t.Fatalf("build grafana dashboard request: %v", err)
	}
	req.SetBasicAuth("admin", "admin")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("grafana dashboard request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("grafana dashboard status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload grafanaDashboardResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode grafana dashboard response: %v", err)
	}
	return payload.Dashboard
}

func waitForPrometheusValue(t *testing.T, promURL, query string, timeout time.Duration) float64 {
	t.Helper()

	deadline := time.Now().Add(timeout)
	lastErr := ""
	for time.Now().Before(deadline) {
		value, err := queryPrometheusValue(promURL, query)
		if err == nil && value > 0 {
			return value
		}
		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = fmt.Sprintf("query returned %.3f", value)
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("timed out waiting for Prometheus query %q: %s", query, lastErr)
	return 0
}

func queryPrometheusValue(promURL, query string) (float64, error) {
	req, err := http.NewRequest(http.MethodGet, promURL+"/api/v1/query", nil)
	if err != nil {
		return 0, err
	}
	q := req.URL.Query()
	q.Set("query", query)
	req.URL.RawQuery = q.Encode()

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload prometheusQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	if payload.Status != "success" {
		return 0, fmt.Errorf("status=%s", payload.Status)
	}
	if len(payload.Data.Result) == 0 {
		return 0, nil
	}
	if len(payload.Data.Result[0].Value) < 2 {
		return 0, fmt.Errorf("unexpected result shape")
	}
	raw := fmt.Sprint(payload.Data.Result[0].Value[1])
	var value float64
	if _, err := fmt.Sscanf(raw, "%f", &value); err != nil {
		return 0, fmt.Errorf("parse sample value %q: %w", raw, err)
	}
	return value, nil
}

func startPortForward(t *testing.T, namespace, service string, remotePort int) string {
	t.Helper()

	localPort := freeLocalPort(t)
	target := fmt.Sprintf("%d:%d", localPort, remotePort)
	cmd := exec.Command("kubectl", "-n", namespace, "port-forward", "service/"+service, target)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kubectl port-forward for %s/%s: %v", namespace, service, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	address := fmt.Sprintf("127.0.0.1:%d", localPort)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = conn.Close()
			return "http://" + address
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("port-forward for %s/%s did not become ready: %s", namespace, service, strings.TrimSpace(stderr.String()))
	return ""
}

func freeLocalPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate local port: %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("unexpected listener address type")
	}
	return addr.Port
}
