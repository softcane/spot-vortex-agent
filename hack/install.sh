#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-spotvortex}"
RELEASE_NAME="${RELEASE_NAME:-spotvortex}"
CHART_REF="${CHART_REF:-oci://ghcr.io/softcane/charts/spotvortex}"
CHART_VERSION="${CHART_VERSION:-}"
HELM_TIMEOUT="${HELM_TIMEOUT:-5m}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-}"
IMAGE_TAG="${IMAGE_TAG:-}"
MODE_DRY_RUN="${MODE_DRY_RUN:-}"

if ! command -v helm >/dev/null 2>&1; then
  echo "helm is required"
  exit 1
fi

HELM_ARGS=(
  upgrade --install "${RELEASE_NAME}" "${CHART_REF}"
  --namespace "${NAMESPACE}"
  --create-namespace
  --wait
  --timeout "${HELM_TIMEOUT}"
)

if [[ -n "${CHART_VERSION}" ]]; then
  HELM_ARGS+=(--version "${CHART_VERSION}")
fi
if [[ -n "${IMAGE_REPOSITORY}" ]]; then
  HELM_ARGS+=(--set-string "agent.image.repository=${IMAGE_REPOSITORY}")
fi
if [[ -n "${IMAGE_TAG}" ]]; then
  HELM_ARGS+=(--set-string "agent.image.tag=${IMAGE_TAG}")
fi
if [[ -n "${MODE_DRY_RUN}" ]]; then
  HELM_ARGS+=(--set "mode.dryRun=${MODE_DRY_RUN}")
fi

helm "${HELM_ARGS[@]}"

echo "SpotVortex installed: release=${RELEASE_NAME} namespace=${NAMESPACE} chart=${CHART_REF} version=${CHART_VERSION:-latest}"
