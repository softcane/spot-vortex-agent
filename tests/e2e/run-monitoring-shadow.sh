#!/usr/bin/env bash
# Runs the monitoring-focused Kind validation:
# - fresh Kind cluster with monitoring stack
# - local Helm install of the current agent image
# - synthetic node metrics + fake price provider for deterministic dry-run behavior
# - live Grafana + Prometheus assertions

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-spotvortex-e2e}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-spotvortex-agent}"
IMAGE_TAG="${IMAGE_TAG:-monitoring-e2e}"
HELM_RELEASE="${HELM_RELEASE:-spotvortex}"
HELM_NAMESPACE="${HELM_NAMESPACE:-spotvortex}"
FAKE_PRICE_JSON="${FAKE_PRICE_JSON:-{\"default\":{\"current_price\":0.20,\"on_demand_price\":1.00,\"price_history\":[0.20,0.19,0.21]}}}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

log() {
  echo "[monitoring-shadow] $*"
}

label_nodes_for_runtime_scope() {
  local worker_index=0
  while IFS= read -r node; do
    [[ -z "$node" ]] && continue

    local capacity_type="on-demand"
    if ! kubectl get node "$node" -o jsonpath='{.metadata.labels.node-role\.kubernetes\.io/control-plane}' | grep -q . &&
      ! kubectl get node "$node" -o jsonpath='{.metadata.labels.node-role\.kubernetes\.io/master}' | grep -q .; then
      if [[ "$worker_index" -eq 0 ]]; then
        capacity_type="spot"
      fi
      worker_index=$((worker_index + 1))
    fi

    kubectl label node "$node" \
      node.kubernetes.io/instance-type=m5.large \
      topology.kubernetes.io/zone=us-east-1a \
      karpenter.sh/capacity-type="$capacity_type" \
      spotvortex.io/managed=true \
      spotvortex.io/pool=general \
      --overwrite >/dev/null
  done < <(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
}

install_local_chart() {
  log "Building local agent image ${IMAGE_REPOSITORY}:${IMAGE_TAG}"
  docker build --build-arg "VERSION=${IMAGE_TAG}" -t "${IMAGE_REPOSITORY}:${IMAGE_TAG}" "$PROJECT_ROOT" >/dev/null

  log "Loading image into Kind cluster ${CLUSTER_NAME}"
  kind load docker-image "${IMAGE_REPOSITORY}:${IMAGE_TAG}" --name "${CLUSTER_NAME}" >/dev/null

  log "Installing Helm chart into namespace ${HELM_NAMESPACE}"
  helm upgrade --install "${HELM_RELEASE}" "$PROJECT_ROOT/charts/spotvortex" \
    --namespace "${HELM_NAMESPACE}" \
    --create-namespace \
    --wait \
    --timeout 5m \
    --set "agent.image.repository=${IMAGE_REPOSITORY}" \
    --set "agent.image.tag=${IMAGE_TAG}" \
    --set "prometheus.url=http://prometheus.monitoring.svc.cluster.local:9090" >/dev/null

  log "Enabling synthetic metrics and fake prices for local monitoring proof"
  kubectl -n "${HELM_NAMESPACE}" set env deployment/"${HELM_RELEASE}"-agent \
    SPOTVORTEX_METRICS_MODE=synthetic \
    SPOTVORTEX_E2E_SUITE=monitoring \
    SPOTVORTEX_TEST_PRICE_PROVIDER_JSON="${FAKE_PRICE_JSON}" >/dev/null

  kubectl -n "${HELM_NAMESPACE}" rollout status deployment/"${HELM_RELEASE}"-agent --timeout=5m >/dev/null
}

main() {
  require_cmd go
  require_cmd docker
  require_cmd helm
  require_cmd kind
  require_cmd kubectl

  log "Creating fresh Kind cluster with monitoring stack"
  INSTALL_MONITORING=1 USE_KWOK=0 bash "$SCRIPT_DIR/setup.sh"

  log "Labelling nodes into supported runtime scope"
  label_nodes_for_runtime_scope

  install_local_chart

  log "Running live Grafana and Prometheus assertions"
  SPOTVORTEX_E2E_SUITE=monitoring \
    go test -v ./tests/e2e -run 'TestMonitoring_' -count=1
}

main "$@"
