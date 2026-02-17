#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-spotvortex}"
RELEASE_NAME="${RELEASE_NAME:-spotvortex}"
CHART_REF="${CHART_REF:-oci://ghcr.io/spotvortex/charts/spotvortex}"
API_KEY="${SPOTVORTEX_API_KEY:-${API_KEY:-}}"

if [[ -z "${API_KEY}" ]]; then
  echo "SPOTVORTEX_API_KEY (or API_KEY) is required"
  echo "Example: SPOTVORTEX_API_KEY=sv_xxx bash install.sh"
  exit 1
fi

if ! command -v helm >/dev/null 2>&1; then
  echo "helm is required"
  exit 1
fi

helm upgrade --install "${RELEASE_NAME}" "${CHART_REF}" \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  --set apiKey="${API_KEY}"

echo "SpotVortex installed: release=${RELEASE_NAME} namespace=${NAMESPACE}"
