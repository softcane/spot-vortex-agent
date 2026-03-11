#!/usr/bin/env bash
set -euo pipefail

CHART_SOURCE_DIR="${CHART_SOURCE_DIR:-charts/spotvortex}"
OUTPUT_DIR="${OUTPUT_DIR:-dist}"
RELEASE_VERSION="${RELEASE_VERSION:-}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/softcane/spot-vortex-agent}"

if [[ ! -f "${CHART_SOURCE_DIR}/Chart.yaml" ]]; then
  echo "Chart.yaml not found in ${CHART_SOURCE_DIR}" >&2
  exit 1
fi
if [[ ! -f "${CHART_SOURCE_DIR}/values.yaml" ]]; then
  echo "values.yaml not found in ${CHART_SOURCE_DIR}" >&2
  exit 1
fi

chart_version_from_source() {
  awk -F': ' '/^version:/ { gsub(/"/, "", $2); print $2; exit }' "${CHART_SOURCE_DIR}/Chart.yaml"
}

app_version_from_source() {
  awk -F': ' '/^appVersion:/ { gsub(/"/, "", $2); print $2; exit }' "${CHART_SOURCE_DIR}/Chart.yaml"
}

stage_chart_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${stage_chart_dir}"
}
trap cleanup EXIT

cp -R "${CHART_SOURCE_DIR}" "${stage_chart_dir}/spotvortex"
staged_chart_dir="${stage_chart_dir}/spotvortex"

chart_version="$(chart_version_from_source)"
app_version="$(app_version_from_source)"
if [[ -n "${RELEASE_VERSION}" ]]; then
  chart_version="${RELEASE_VERSION#v}"
  app_version="${RELEASE_VERSION}"
fi

awk -v chart_version="${chart_version}" -v app_version="${app_version}" '
  /^version:/ { print "version: " chart_version; next }
  /^appVersion:/ { print "appVersion: \"" app_version "\""; next }
  { print }
' "${staged_chart_dir}/Chart.yaml" > "${staged_chart_dir}/Chart.yaml.tmp"
mv "${staged_chart_dir}/Chart.yaml.tmp" "${staged_chart_dir}/Chart.yaml"

awk -v image_repository="${IMAGE_REPOSITORY}" '
  /^agent:[[:space:]]*$/ { in_agent = 1; print; next }
  in_agent && /^  image:[[:space:]]*$/ { in_image = 1; print; next }
  in_image && /^    repository:/ { print "    repository: " image_repository; next }
  in_image && /^  [^[:space:]]/ { in_image = 0; in_agent = 0 }
  { print }
' "${staged_chart_dir}/values.yaml" > "${staged_chart_dir}/values.yaml.tmp"
mv "${staged_chart_dir}/values.yaml.tmp" "${staged_chart_dir}/values.yaml"

mkdir -p "${OUTPUT_DIR}"
helm dependency update "${staged_chart_dir}" >/dev/null || true
helm package "${staged_chart_dir}" --destination "${OUTPUT_DIR}" >/dev/null

echo "Packaged chart: ${OUTPUT_DIR}/spotvortex-${chart_version}.tgz"
