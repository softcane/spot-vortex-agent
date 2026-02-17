#!/usr/bin/env bash
# SpotVortex E2E setup for real EKS Anywhere (Docker provider) + Cluster Autoscaler.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

WORK_DIR="${EKSA_WORK_DIR:-$PROJECT_ROOT/.tmp/eksa-ca}"
MGMT_CLUSTER_NAME="${MGMT_CLUSTER_NAME:-spotvortex-eksa-mgmt}"
WORKLOAD_CLUSTER_NAME="${WORKLOAD_CLUSTER_NAME:-spotvortex-eksa-ca}"

MANIFEST_URL="https://anywhere-assets.eks.amazonaws.com/releases/eks-a/manifest.yaml"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

log() {
  echo "[eksa-ca] $*"
}

generate_cluster_config() {
  local cluster_name="$1"
  local output_file="$2"

  log "Generating cluster config: $cluster_name"
  eksctl anywhere generate clusterconfig "$cluster_name" --provider docker >"$output_file"
}

patch_workload_cluster_config() {
  local workload_cfg="$1"

  log "Patching workload cluster config to use management cluster: $MGMT_CLUSTER_NAME"
  MGMT_CLUSTER_NAME="$MGMT_CLUSTER_NAME" yq -i \
    'select(.kind == "Cluster").spec.managementCluster.name = env(MGMT_CLUSTER_NAME)' \
    "$workload_cfg"
}

set_cluster_cni_kindnetd() {
  local cluster_cfg="$1"

  # Use kindnetd for local Docker runs to avoid Cilium bootstrap deadlocks on constrained hosts.
  yq -i \
    'select(.kind == "Cluster").spec.clusterNetwork.cniConfig = {"kindnetd": {}}' \
    "$cluster_cfg"
}

derive_eksa_release() {
  local release_branch
  release_branch="$(curl -fsSL "$MANIFEST_URL" | yq eval -r '.spec.latestVersion' -)"
  if [[ -z "$release_branch" || "$release_branch" == "null" ]]; then
    echo "Failed to resolve RELEASE_BRANCH from $MANIFEST_URL" >&2
    exit 1
  fi

  EKS_RELEASE="$(curl -fsSL "$MANIFEST_URL" | yq eval -r ".spec.releases[] | select(.branch==\"$release_branch\") | .latestVersion" -)"
  if [[ -z "$EKS_RELEASE" || "$EKS_RELEASE" == "null" ]]; then
    echo "Failed to resolve EKS_RELEASE for branch $release_branch" >&2
    exit 1
  fi

  export EKS_RELEASE
  log "Resolved EKS Anywhere release: $EKS_RELEASE (branch: $release_branch)"
}

install_eksa_package_components() {
  local mgmt_kubeconfig="$1"

  derive_eksa_release

  log "Installing EKS Anywhere package components in management cluster"
  kubectl --kubeconfig "$mgmt_kubeconfig" create namespace eksa-packages --dry-run=client -o yaml | \
    kubectl --kubeconfig "$mgmt_kubeconfig" apply -f -

  kubectl --kubeconfig "$mgmt_kubeconfig" apply -f "$MANIFEST_URL"
  kubectl --kubeconfig "$mgmt_kubeconfig" apply -f \
    "https://anywhere-assets.eks.amazonaws.com/releases/eks-a/${EKS_RELEASE}/manifests/eksa-packages-${EKS_RELEASE//./-}.yaml"

  if ! kubectl --kubeconfig "$mgmt_kubeconfig" -n eksa-packages get packagebundlecontroller bundle-controller >/dev/null 2>&1; then
    kubectl --kubeconfig "$mgmt_kubeconfig" apply -f - <<'EOF'
apiVersion: packages.eks.amazonaws.com/v1alpha1
kind: PackageBundleController
metadata:
  name: bundle-controller
  namespace: eksa-packages
spec:
  logLevel: 4
  upgradeCheckInterval: 24h
EOF
  fi
}

install_cluster_autoscaler_package() {
  local mgmt_kubeconfig="$1"

  log "Installing Cluster Autoscaler package for workload cluster: $WORKLOAD_CLUSTER_NAME"
  kubectl --kubeconfig "$mgmt_kubeconfig" apply -f - <<EOF
apiVersion: packages.eks.amazonaws.com/v1alpha1
kind: Package
metadata:
  name: cluster-autoscaler
  namespace: eksa-packages
spec:
  packageName: cluster-autoscaler.eksa.aws
  targetNamespace: kube-system
  targetCluster:
    kind: Cluster
    name: ${WORKLOAD_CLUSTER_NAME}
  config: |
    clusterName: ${WORKLOAD_CLUSTER_NAME}
EOF
}

wait_for_cluster_autoscaler_deploy() {
  local workload_kubeconfig="$1"
  local deployment_name=""

  log "Waiting for Cluster Autoscaler deployment in workload cluster"
  for _ in $(seq 1 60); do
    deployment_name="$(kubectl --kubeconfig "$workload_kubeconfig" -n kube-system get deploy -o name | grep 'cluster-autoscaler' | head -n1 || true)"
    if [[ -n "$deployment_name" ]]; then
      kubectl --kubeconfig "$workload_kubeconfig" -n kube-system rollout status "$deployment_name" --timeout=10m
      return 0
    fi
    sleep 10
  done

  echo "Timed out waiting for cluster-autoscaler deployment to appear in kube-system" >&2
  kubectl --kubeconfig "$workload_kubeconfig" -n kube-system get deploy || true
  exit 1
}

label_worker_nodes_for_capacity_routing() {
  local workload_kubeconfig="$1"

  log "Labelling workload worker nodes for SpotVortex capacity routing"
  local nodes
  nodes="$(kubectl --kubeconfig "$workload_kubeconfig" get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"

  while IFS= read -r node; do
    [[ -z "$node" ]] && continue

    if kubectl --kubeconfig "$workload_kubeconfig" get node "$node" -o jsonpath='{.metadata.labels.node-role\.kubernetes\.io/control-plane}' | grep -q .; then
      continue
    fi
    if kubectl --kubeconfig "$workload_kubeconfig" get node "$node" -o jsonpath='{.metadata.labels.node-role\.kubernetes\.io/master}' | grep -q .; then
      continue
    fi

    kubectl --kubeconfig "$workload_kubeconfig" label node "$node" \
      spotvortex.io/manager=cluster-autoscaler \
      spotvortex.io/pool=eksa-ca-pool \
      spotvortex.io/managed=true \
      --overwrite
  done <<<"$nodes"
}

main() {
  require_cmd docker
  require_cmd kubectl
  require_cmd eksctl
  require_cmd eksctl-anywhere
  require_cmd kind
  require_cmd clusterctl
  require_cmd yq
  require_cmd curl

  mkdir -p "$WORK_DIR"

  local mgmt_cfg="$WORK_DIR/${MGMT_CLUSTER_NAME}.yaml"
  local workload_cfg="$WORK_DIR/${WORKLOAD_CLUSTER_NAME}.yaml"
  local mgmt_kubeconfig="$WORK_DIR/${MGMT_CLUSTER_NAME}/${MGMT_CLUSTER_NAME}-eks-a-cluster.kubeconfig"
  local workload_kubeconfig="$WORK_DIR/${WORKLOAD_CLUSTER_NAME}/${WORKLOAD_CLUSTER_NAME}-eks-a-cluster.kubeconfig"

  pushd "$WORK_DIR" >/dev/null

  if [[ ! -f "$mgmt_kubeconfig" ]]; then
    generate_cluster_config "$MGMT_CLUSTER_NAME" "$mgmt_cfg"
    set_cluster_cni_kindnetd "$mgmt_cfg"
    log "Creating management cluster: $MGMT_CLUSTER_NAME"
    eksctl anywhere create cluster -f "$mgmt_cfg"
  else
    log "Management kubeconfig already exists, reusing cluster: $MGMT_CLUSTER_NAME"
  fi

  if [[ ! -f "$workload_kubeconfig" ]]; then
    generate_cluster_config "$WORKLOAD_CLUSTER_NAME" "$workload_cfg"
    patch_workload_cluster_config "$workload_cfg"
    set_cluster_cni_kindnetd "$workload_cfg"
    log "Creating workload cluster: $WORKLOAD_CLUSTER_NAME"
    KUBECONFIG="$mgmt_kubeconfig" eksctl anywhere create cluster -f "$workload_cfg"
  else
    log "Workload kubeconfig already exists, reusing cluster: $WORKLOAD_CLUSTER_NAME"
  fi

  popd >/dev/null

  install_eksa_package_components "$mgmt_kubeconfig"
  install_cluster_autoscaler_package "$mgmt_kubeconfig"
  wait_for_cluster_autoscaler_deploy "$workload_kubeconfig"
  label_worker_nodes_for_capacity_routing "$workload_kubeconfig"

  echo "MANAGEMENT_KUBECONFIG=$mgmt_kubeconfig"
  echo "WORKLOAD_KUBECONFIG=$workload_kubeconfig"
}

main "$@"
