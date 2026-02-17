#!/usr/bin/env bash
# SpotVortex E2E setup for local Karpenter integration checks.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-spotvortex-e2e}"
KIND_CONFIG="${KIND_CONFIG:-$SCRIPT_DIR/kind-config.yaml}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

log() {
  echo "[karpenter-local] $*"
}

create_or_reset_cluster() {
  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log "Deleting existing kind cluster: $CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
  fi

  log "Creating kind cluster: $CLUSTER_NAME"
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG"
}

wait_for_nodes() {
  log "Waiting for nodes to become Ready"
  kubectl wait --for=condition=Ready nodes --all --timeout=120s
}

install_local_karpenter_crds() {
  log "Installing local Karpenter NodePool CRD"
  kubectl apply -f "$SCRIPT_DIR/manifests/karpenter-nodepool-crd.yaml"
  kubectl wait --for=condition=Established crd/nodepools.karpenter.sh --timeout=60s
}

install_local_nodepools() {
  log "Applying local spot/on-demand NodePools"
  kubectl apply -f "$SCRIPT_DIR/manifests/karpenter-local-nodepools.yaml"
}

label_worker_nodes() {
  log "Labelling worker nodes for SpotVortex pool ownership"
  while IFS= read -r node; do
    [[ -z "$node" ]] && continue
    if kubectl get node "$node" -o jsonpath='{.metadata.labels.node-role\.kubernetes\.io/control-plane}' | grep -q .; then
      continue
    fi
    if kubectl get node "$node" -o jsonpath='{.metadata.labels.node-role\.kubernetes\.io/master}' | grep -q .; then
      continue
    fi

    kubectl label node "$node" \
      spotvortex.io/pool=general \
      spotvortex.io/managed=true \
      --overwrite >/dev/null
  done < <(kubectl get nodes -o name | sed 's|^node/||')
}

main() {
  require_cmd kind
  require_cmd kubectl

  create_or_reset_cluster
  wait_for_nodes
  install_local_karpenter_crds
  install_local_nodepools
  label_worker_nodes

  log "Setup complete"
  echo "KUBECONFIG=${KUBECONFIG:-$HOME/.kube/config}"
  echo "CLUSTER_NAME=$CLUSTER_NAME"
}

main "$@"
