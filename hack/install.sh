#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-spotvortex}"
RELEASE_NAME="${RELEASE_NAME:-spotvortex}"
CHART_REF="${CHART_REF:-oci://ghcr.io/spotvortex/charts/spotvortex}"
API_KEY="${SPOTVORTEX_API_KEY:-${API_KEY:-}}"

if ! command -v helm >/dev/null 2>&1; then
  echo "helm is required"
  exit 1
fi

HELM_ARGS=(
  upgrade --install "${RELEASE_NAME}" "${CHART_REF}"
  --namespace "${NAMESPACE}"
  --create-namespace
)

if [[ -n "${API_KEY}" ]]; then
  HELM_ARGS+=(--set "apiKey=${API_KEY}")
fi

helm "${HELM_ARGS[@]}"

echo "SpotVortex installed: release=${RELEASE_NAME} namespace=${NAMESPACE}"
