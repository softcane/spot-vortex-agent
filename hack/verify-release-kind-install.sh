#!/usr/bin/env bash
# Verifies release installation paths against a local Kind cluster:
# 1) direct Helm install
# 2) hack/install.sh install
#
# Modes:
# - VERIFY_MODE=local (default): uses the in-repo chart plus a local image loaded into Kind.
# - VERIFY_MODE=published: uses the published OCI chart and requires an explicit CHART_VERSION.

set -euo pipefail

readonly LOCAL_CHART_REF_DEFAULT="charts/spotvortex"
readonly PUBLISHED_CHART_REF_DEFAULT="oci://ghcr.io/softcane/spot-vortex-charts/spotvortex"

VERIFY_MODE="${VERIFY_MODE:-local}"

CLUSTER_NAME="${CLUSTER_NAME:-spotvortex-release-verify}"
CREATE_CLUSTER="${CREATE_CLUSTER:-1}"
DELETE_CLUSTER_ON_EXIT="${DELETE_CLUSTER_ON_EXIT:-1}"
KIND_LOAD_IMAGE="${KIND_LOAD_IMAGE:-}"
BUILD_LOCAL_IMAGE="${BUILD_LOCAL_IMAGE:-}"
LOCAL_IMAGE_REPOSITORY="${LOCAL_IMAGE_REPOSITORY:-spotvortex-agent}"
LOCAL_IMAGE_TAG="${LOCAL_IMAGE_TAG:-verify-local}"
IMAGE_BUILD_VERSION="${IMAGE_BUILD_VERSION:-}"

CHART_REF="${CHART_REF:-}"
CHART_VERSION="${CHART_VERSION:-}"
HELM_TIMEOUT="${HELM_TIMEOUT:-5m}"
INSTALL_SCRIPT_MODE="${INSTALL_SCRIPT_MODE:-local}"
INSTALL_SCRIPT_URL="${INSTALL_SCRIPT_URL:-https://raw.githubusercontent.com/softcane/spot-vortex-agent/main/hack/install.sh}"

EXPECTED_IMAGE_REPOSITORY="${EXPECTED_IMAGE_REPOSITORY:-}"
EXPECTED_IMAGE_TAG="${EXPECTED_IMAGE_TAG:-}"
EXPECTED_ORT_LIBRARY_PATH="${EXPECTED_ORT_LIBRARY_PATH:-}"
FORCE_IMAGE_OVERRIDE="${FORCE_IMAGE_OVERRIDE:-0}"
IMAGE_PULL_SECRET_NAME="${IMAGE_PULL_SECRET_NAME:-}"

HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-spotvortex-helm}"
HELM_NAMESPACE="${HELM_NAMESPACE:-spotvortex-helm}"

SCRIPT_RELEASE_NAME="${SCRIPT_RELEASE_NAME:-spotvortex-script}"
SCRIPT_NAMESPACE="${SCRIPT_NAMESPACE:-spotvortex-script}"

log() {
  echo "[verify-release] $*"
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

configure_mode_defaults() {
  case "${VERIFY_MODE}" in
    local)
      CHART_REF="${CHART_REF:-${LOCAL_CHART_REF_DEFAULT}}"
      EXPECTED_IMAGE_REPOSITORY="${EXPECTED_IMAGE_REPOSITORY:-${LOCAL_IMAGE_REPOSITORY}}"
      EXPECTED_IMAGE_TAG="${EXPECTED_IMAGE_TAG:-${LOCAL_IMAGE_TAG}}"
      BUILD_LOCAL_IMAGE="${BUILD_LOCAL_IMAGE:-1}"
      FORCE_IMAGE_OVERRIDE="1"
      if [[ -z "${KIND_LOAD_IMAGE}" ]]; then
        KIND_LOAD_IMAGE="${EXPECTED_IMAGE_REPOSITORY}:${EXPECTED_IMAGE_TAG}"
      fi
      if [[ -z "${IMAGE_BUILD_VERSION}" ]]; then
        IMAGE_BUILD_VERSION="${EXPECTED_IMAGE_TAG}"
      fi
      ;;
    published)
      CHART_REF="${CHART_REF:-${PUBLISHED_CHART_REF_DEFAULT}}"
      BUILD_LOCAL_IMAGE="${BUILD_LOCAL_IMAGE:-0}"
      if [[ -z "${CHART_VERSION}" ]]; then
        echo "VERIFY_MODE=published requires CHART_VERSION so Helm does not resolve an unexpected OCI chart version." >&2
        exit 1
      fi
      ;;
    *)
      echo "Unsupported VERIFY_MODE=${VERIFY_MODE}. Use local or published." >&2
      exit 1
      ;;
  esac
}

build_local_image_if_needed() {
  if [[ "${BUILD_LOCAL_IMAGE}" != "1" ]]; then
    return 0
  fi
  if [[ -z "${KIND_LOAD_IMAGE}" ]]; then
    echo "BUILD_LOCAL_IMAGE=1 requires KIND_LOAD_IMAGE" >&2
    exit 1
  fi

  require_cmd docker
  log "Building local image for Kind verification: ${KIND_LOAD_IMAGE}"
  docker build --build-arg "VERSION=${IMAGE_BUILD_VERSION}" -t "${KIND_LOAD_IMAGE}" . >/dev/null
}

cleanup() {
  if [[ "${DELETE_CLUSTER_ON_EXIT}" == "1" ]]; then
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
      log "Deleting kind cluster: ${CLUSTER_NAME}"
      kind delete cluster --name "${CLUSTER_NAME}" >/dev/null
    fi
  fi
}

deployment_for_release() {
  local namespace="$1"
  local release="$2"

  kubectl -n "${namespace}" get deploy \
    -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" \
    -o jsonpath='{.items[0].metadata.name}'
}

assert_pod_stability() {
  local namespace="$1"
  local release="$2"

  # Wait briefly after rollout success to catch immediate crash loops.
  sleep 8

  local podRows
  podRows="$(kubectl -n "${namespace}" get pods \
    -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.status.phase}{"|"}{.status.containerStatuses[0].ready}{"|"}{.status.containerStatuses[0].restartCount}{"\n"}{end}')"
  if [[ -z "${podRows}" ]]; then
    echo "No agent pods found for release=${release} namespace=${namespace}" >&2
    exit 1
  fi

  local failures=0
  while IFS='|' read -r pod phase ready restarts; do
    [[ -z "${pod}" ]] && continue
    if [[ "${phase}" != "Running" ]]; then
      echo "Pod not running: ${pod} phase=${phase}" >&2
      failures=1
    fi
    if [[ "${ready}" != "true" ]]; then
      echo "Pod not ready: ${pod} ready=${ready}" >&2
      failures=1
    fi
    if [[ -z "${restarts}" || "${restarts}" != "0" ]]; then
      echo "Pod restart detected: ${pod} restarts=${restarts}" >&2
      failures=1
    fi
  done <<< "${podRows}"

  if [[ "${failures}" != "0" ]]; then
    echo "Agent pod stability assertion failed for release=${release} namespace=${namespace}" >&2
    kubectl -n "${namespace}" get pods -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" -o wide >&2 || true
    kubectl -n "${namespace}" logs -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" --tail=80 >&2 || true
    exit 1
  fi
}

assert_shadow_defaults() {
  local namespace="$1"
  local release="$2"

  local deploy
  deploy="$(deployment_for_release "${namespace}" "${release}")"
  if [[ -z "${deploy}" ]]; then
    echo "No agent deployment found for release=${release} namespace=${namespace}" >&2
    exit 1
  fi

  if ! kubectl -n "${namespace}" rollout status "deployment/${deploy}" --timeout="${HELM_TIMEOUT}" >/dev/null; then
    kubectl -n "${namespace}" get pods -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" -o wide >&2 || true
    kubectl -n "${namespace}" describe deployment "${deploy}" >&2 || true
    kubectl -n "${namespace}" describe pods -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" >&2 || true
    kubectl -n "${namespace}" logs -l "app.kubernetes.io/instance=${release},app.kubernetes.io/component=agent" --tail=80 >&2 || true
    exit 1
  fi
  assert_pod_stability "${namespace}" "${release}"

  local args
  args="$(kubectl -n "${namespace}" get deployment "${deploy}" -o jsonpath='{.spec.template.spec.containers[0].args[*]}')"
  if [[ "${args}" != *"--dry-run"* ]]; then
    echo "Expected shadow mode arg (--dry-run) for ${deploy}, got: ${args}" >&2
    exit 1
  fi
  if [[ "${args}" == *"--dry-run=false"* ]]; then
    echo "Expected default shadow mode, but found --dry-run=false in ${deploy}" >&2
    exit 1
  fi

  local image
  image="$(kubectl -n "${namespace}" get deployment "${deploy}" -o jsonpath='{.spec.template.spec.containers[0].image}')"
  if [[ -z "${image}" ]]; then
    echo "Container image is empty for ${deploy}" >&2
    exit 1
  fi
  if [[ -n "${EXPECTED_IMAGE_REPOSITORY}" && -n "${EXPECTED_IMAGE_TAG}" ]]; then
    local expected_image="${EXPECTED_IMAGE_REPOSITORY}:${EXPECTED_IMAGE_TAG}"
    if [[ "${image}" != "${expected_image}" ]]; then
      echo "Unexpected image for ${deploy}. got=${image} want=${expected_image}" >&2
      exit 1
    fi
  fi

  local ortPath
  ortPath="$(kubectl -n "${namespace}" get deployment "${deploy}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SPOTVORTEX_ONNXRUNTIME_PATH")].value}')"
  if [[ -z "${ortPath}" ]]; then
    echo "SPOTVORTEX_ONNXRUNTIME_PATH is missing for ${deploy}" >&2
    exit 1
  fi
  if [[ -n "${EXPECTED_ORT_LIBRARY_PATH}" && "${ortPath}" != "${EXPECTED_ORT_LIBRARY_PATH}" ]]; then
    echo "Unexpected SPOTVORTEX_ONNXRUNTIME_PATH for ${deploy}. got=${ortPath} want=${EXPECTED_ORT_LIBRARY_PATH}" >&2
    exit 1
  fi

  local fullname="${deploy%-agent}"
  if kubectl -n "${namespace}" get secret "${fullname}-api-key" >/dev/null 2>&1; then
    echo "API key secret should not exist by default for ${release}" >&2
    exit 1
  fi

  local cfg
  cfg="$(kubectl -n "${namespace}" get configmap "${fullname}-config" -o jsonpath='{.data.config\.yaml}')"
  if ! printf '%s' "${cfg}" | grep -q 'expectedCloud: "aws"'; then
    echo "expectedCloud is not set to aws in rendered config for ${release}" >&2
    exit 1
  fi
  if ! printf '%s\n' "${cfg}" | awk '
    /^[[:space:]]*karpenter:[[:space:]]*$/ { in_karpenter = 1; next }
    in_karpenter && /^[[:space:]]*enabled:[[:space:]]*false([[:space:]]|$)/ { found = 1; exit 0 }
    in_karpenter && /^[^[:space:]]/ { in_karpenter = 0 }
    END { exit found ? 0 : 1 }
  '; then
    echo "expected default karpenter.enabled=false for install compatibility in ${release}" >&2
    exit 1
  fi

  log "Assertions passed for release=${release} namespace=${namespace}"
}

install_via_helm() {
  log "Installing via Helm (mode=${VERIFY_MODE} release=${HELM_RELEASE_NAME} chart=${CHART_REF})"
  local helmArgs=(
    upgrade --install "${HELM_RELEASE_NAME}" "${CHART_REF}"
    --namespace "${HELM_NAMESPACE}"
    --create-namespace
    --wait
    --timeout "${HELM_TIMEOUT}"
  )
  if [[ -n "${CHART_VERSION}" ]]; then
    helmArgs+=(--version "${CHART_VERSION}")
  fi
  if [[ "${FORCE_IMAGE_OVERRIDE}" == "1" ]]; then
    if [[ -z "${EXPECTED_IMAGE_REPOSITORY}" || -z "${EXPECTED_IMAGE_TAG}" ]]; then
      echo "FORCE_IMAGE_OVERRIDE=1 requires EXPECTED_IMAGE_REPOSITORY and EXPECTED_IMAGE_TAG" >&2
      exit 1
    fi
    helmArgs+=(--set-string "agent.image.repository=${EXPECTED_IMAGE_REPOSITORY}")
    helmArgs+=(--set-string "agent.image.tag=${EXPECTED_IMAGE_TAG}")
  fi
  if [[ -n "${IMAGE_PULL_SECRET_NAME}" ]]; then
    helmArgs+=(--set-string "agent.image.pullSecrets[0]=${IMAGE_PULL_SECRET_NAME}")
  fi
  helm "${helmArgs[@]}"
}

install_via_script() {
  if [[ "${INSTALL_SCRIPT_MODE}" == "download" ]]; then
    log "Installing via downloaded install.sh (release=${SCRIPT_RELEASE_NAME})"
  else
    log "Installing via hack/install.sh (release=${SCRIPT_RELEASE_NAME})"
  fi

  local imageRepo=""
  local imageTag=""
  if [[ "${FORCE_IMAGE_OVERRIDE}" == "1" ]]; then
    imageRepo="${EXPECTED_IMAGE_REPOSITORY}"
    imageTag="${EXPECTED_IMAGE_TAG}"
  fi

  local installScriptPath="hack/install.sh"
  local tmpScript=""
  if [[ "${INSTALL_SCRIPT_MODE}" == "download" ]]; then
    tmpScript="$(mktemp)"
    curl -fsSL "${INSTALL_SCRIPT_URL}" -o "${tmpScript}"
    chmod +x "${tmpScript}"
    installScriptPath="${tmpScript}"
  fi

  NAMESPACE="${SCRIPT_NAMESPACE}" \
  RELEASE_NAME="${SCRIPT_RELEASE_NAME}" \
  CHART_REF="${CHART_REF}" \
  CHART_VERSION="${CHART_VERSION}" \
  HELM_TIMEOUT="${HELM_TIMEOUT}" \
  IMAGE_REPOSITORY="${imageRepo}" \
  IMAGE_TAG="${imageTag}" \
  IMAGE_PULL_SECRET_NAME="${IMAGE_PULL_SECRET_NAME}" \
    bash "${installScriptPath}"

  if [[ -n "${tmpScript}" ]]; then
    rm -f "${tmpScript}"
  fi
}

create_cluster_if_needed() {
  if [[ "${CREATE_CLUSTER}" != "1" ]]; then
    log "Skipping cluster creation (CREATE_CLUSTER=${CREATE_CLUSTER})"
    return 0
  fi

  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log "Deleting existing kind cluster: ${CLUSTER_NAME}"
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null
  fi

  log "Creating kind cluster: ${CLUSTER_NAME}"
  kind create cluster --name "${CLUSTER_NAME}" >/dev/null
  kubectl wait --for=condition=Ready nodes --all --timeout=180s >/dev/null

  if [[ -n "${KIND_LOAD_IMAGE}" ]]; then
    log "Loading local image into kind: ${KIND_LOAD_IMAGE}"
    kind load docker-image "${KIND_LOAD_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  fi
}

main() {
  trap cleanup EXIT

  configure_mode_defaults

  require_cmd kind
  require_cmd kubectl
  require_cmd helm
  require_cmd grep
  if [[ "${INSTALL_SCRIPT_MODE}" == "download" ]]; then
    require_cmd curl
  fi

  build_local_image_if_needed
  create_cluster_if_needed

  install_via_helm
  assert_shadow_defaults "${HELM_NAMESPACE}" "${HELM_RELEASE_NAME}"

  install_via_script
  assert_shadow_defaults "${SCRIPT_NAMESPACE}" "${SCRIPT_RELEASE_NAME}"

  log "SUCCESS: Helm install and script install both validated on kind."
}

main "$@"
