#!/bin/bash
# SpotVortex E2E Test Setup Script
# Creates Kind cluster with KWOK fake nodes, test workloads, and monitoring

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CLUSTER_NAME="spotvortex-e2e"
USE_KWOK="${USE_KWOK:-0}"
INSTALL_MONITORING="${INSTALL_MONITORING:-1}"
DRYRUN_DASHBOARD_FILE="$PROJECT_ROOT/dashboards/spotvortex-dryrun.json"
OPS_DASHBOARD_FILE="$PROJECT_ROOT/dashboards/spotvortex-ops-shadow.json"
AGENT_METRICS_SOURCE="${AGENT_METRICS_SOURCE:-service}"

case "$AGENT_METRICS_SOURCE" in
    service|host-endpoints) ;;
    *)
        echo "Error: AGENT_METRICS_SOURCE must be 'service' or 'host-endpoints' (got '$AGENT_METRICS_SOURCE')"
        exit 1
        ;;
esac

echo "=========================================="
echo "SpotVortex E2E Test Environment Setup"
echo "=========================================="

# Check prerequisites
command -v kind >/dev/null 2>&1 || { echo "Error: kind not installed"; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo "Error: kubectl not installed"; exit 1; }

# Delete existing cluster if exists
if kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
    echo "Deleting existing cluster: $CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
fi

# Create cluster
echo ""
echo "Creating Kind cluster: $CLUSTER_NAME"
kind create cluster --name "$CLUSTER_NAME" --config "$SCRIPT_DIR/kind-config.yaml"

# Wait for cluster ready
echo ""
echo "Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=60s

if [ "$USE_KWOK" = "1" ]; then
    # Install KWOK controller
    echo ""
    echo "Installing KWOK for fake nodes..."
    KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
    kubectl apply -k "https://github.com/kubernetes-sigs/kwok/kustomize/kwok?ref=${KWOK_VERSION}"
    kubectl apply -k "https://github.com/kubernetes-sigs/kwok/kustomize/stage/fast?ref=${KWOK_VERSION}"
    kubectl -n kube-system set image deployment/kwok-controller \
        kwok-controller=registry.k8s.io/kwok/kwok:${KWOK_VERSION} >/dev/null

    # Wait for KWOK to be ready
    echo "Waiting for KWOK controller..."
    sleep 5
    kubectl wait --for=condition=Ready pods -l app=kwok-controller -n kube-system --timeout=60s || true

    # Create fake nodes
    echo ""
    echo "Creating fake nodes..."
    kubectl apply -f "$SCRIPT_DIR/manifests/fake-nodes.yaml"

    # Wait for nodes
    echo "Waiting for fake nodes..."
    sleep 3
    kubectl get nodes
fi

# Deploy test workloads
echo ""
echo "Deploying test workloads..."
kubectl apply -f "$SCRIPT_DIR/manifests/test-deployments.yaml"

if [ "$INSTALL_MONITORING" = "1" ]; then
    if [ ! -f "$DRYRUN_DASHBOARD_FILE" ]; then
        echo "Error: dashboard file not found: $DRYRUN_DASHBOARD_FILE"
        exit 1
    fi
    if [ ! -f "$OPS_DASHBOARD_FILE" ]; then
        echo "Error: dashboard file not found: $OPS_DASHBOARD_FILE"
        exit 1
    fi

    echo ""
    echo "Deploying monitoring stack (Prometheus + Grafana)..."
    kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -
    kubectl create namespace spotvortex-system --dry-run=client -o yaml | kubectl apply -f -
    kubectl apply -f "$SCRIPT_DIR/manifests/agent-metrics-basic.yaml"
    if [ "$AGENT_METRICS_SOURCE" = "host-endpoints" ]; then
        echo "Applying opt-in host metrics bridge endpoint (Docker Desktop host -> :8080)"
        kubectl apply -f "$SCRIPT_DIR/manifests/agent-metrics-host-endpoints.yaml"
        echo "WARNING: host-endpoints mode can scrape the wrong local service if :8080 is in use."
    else
        echo "Using safe service-based metrics target (expects in-cluster spotvortex-agent Service endpoints)"
    fi

    echo "Provisioning SpotVortex dashboards into Grafana (dryrun + ops-shadow)..."
    kubectl create configmap spotvortex-dashboards \
        -n monitoring \
        --from-file=spotvortex-dryrun.json="$DRYRUN_DASHBOARD_FILE" \
        --from-file=spotvortex-ops-shadow.json="$OPS_DASHBOARD_FILE" \
        --dry-run=client -o yaml | kubectl apply -f -
    kubectl label configmap spotvortex-dashboards -n monitoring grafana_dashboard=1 --overwrite >/dev/null 2>&1 || true

    kubectl apply -f "$SCRIPT_DIR/manifests/monitoring-stack.yaml"

    echo "Waiting for Prometheus..."
    kubectl -n monitoring rollout status deployment/prometheus --timeout=300s
    echo "Waiting for Grafana..."
    kubectl -n monitoring rollout status deployment/grafana --timeout=300s
fi

# Wait for deployments (pods won't actually run on fake nodes, but manifests are applied)
echo ""
echo "Workloads applied (pods pending on fake nodes - expected)"
kubectl get deployments
kubectl get pods

# Ensure the control-plane node is labeled for capacity-type as a fallback
CONTROL_NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
kubectl label node "$CONTROL_NODE" karpenter.sh/capacity-type=on-demand --overwrite >/dev/null 2>&1 || true

echo ""
echo "=========================================="
echo "E2E Environment Ready!"
echo "=========================================="
echo ""
echo "Cluster: $CLUSTER_NAME"
echo ""
echo "Nodes:"
kubectl get nodes -o wide --show-labels | head -5
echo ""
echo "Pods:"
kubectl get pods -o wide
echo ""
echo "To use this cluster:"
echo "  kubectl config use-context kind-$CLUSTER_NAME"
echo ""
echo "To run E2E tests:"
echo "  go test -v ./tests/e2e/..."
if [ "$INSTALL_MONITORING" = "1" ]; then
    echo ""
    echo "Grafana:"
    echo "  URL: http://localhost:30000"
    echo "  Username: admin"
    echo "  Password: admin"
    echo ""
    echo "If localhost:30000 is not reachable, run:"
    echo "  kubectl -n monitoring port-forward svc/grafana 3000:3000"
    echo ""
    echo "Metrics scrape source mode:"
    echo "  AGENT_METRICS_SOURCE=$AGENT_METRICS_SOURCE"
    echo "  - service (default): scrape in-cluster spotvortex-agent Service endpoints in namespace spotvortex-system"
    echo "  - host-endpoints: opt-in bridge to Docker Desktop host :8080 (be careful with port conflicts)"
fi
echo ""
echo "To cleanup:"
echo "  kind delete cluster --name $CLUSTER_NAME"
