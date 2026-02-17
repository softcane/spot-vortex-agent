#!/usr/bin/env bash
# Runs the local Karpenter shadow-mode validation suite.
#
# Optional:
#   RUN_KARPENTER_PROVIDER_LOCAL=1 to clone aws/karpenter-provider-aws and run `make run`.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORK_DIR="${WORK_DIR:-$PROJECT_ROOT/.tmp/karpenter-local}"
RUN_KARPENTER_PROVIDER_LOCAL="${RUN_KARPENTER_PROVIDER_LOCAL:-0}"
KARPENTER_REPO="${KARPENTER_REPO:-$WORK_DIR/karpenter-provider-aws}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

log() {
  echo "[karpenter-local-run] $*"
}

start_optional_karpenter_provider() {
  if [[ "$RUN_KARPENTER_PROVIDER_LOCAL" != "1" ]]; then
    return 0
  fi

  require_cmd git
  require_cmd make

  mkdir -p "$WORK_DIR"
  if [[ ! -d "$KARPENTER_REPO/.git" ]]; then
    log "Cloning aws/karpenter-provider-aws into $KARPENTER_REPO"
    git clone https://github.com/aws/karpenter-provider-aws "$KARPENTER_REPO"
  fi

  log "Starting Karpenter provider locally with 'make run'"
  (
    cd "$KARPENTER_REPO"
    nohup make run >"$WORK_DIR/karpenter-provider.log" 2>&1 &
    echo $! >"$WORK_DIR/karpenter-provider.pid"
  )
}

stop_optional_karpenter_provider() {
  if [[ -f "$WORK_DIR/karpenter-provider.pid" ]]; then
    local pid
    pid="$(cat "$WORK_DIR/karpenter-provider.pid")"
    if [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1; then
      log "Stopping local Karpenter provider (pid=$pid)"
      kill "$pid" >/dev/null 2>&1 || true
    fi
    rm -f "$WORK_DIR/karpenter-provider.pid"
  fi
}

main() {
  trap stop_optional_karpenter_provider EXIT

  require_cmd go
  require_cmd kind
  require_cmd kubectl

  chmod +x "$SCRIPT_DIR/setup-karpenter-local.sh"
  "$SCRIPT_DIR/setup-karpenter-local.sh"

  start_optional_karpenter_provider

  log "Running karpenter-local shadow-mode assertions"
  SPOTVORTEX_E2E_SUITE=karpenter-local \
    go test -v ./tests/e2e -run 'TestKarpenterLocal_' -count=1
}

main "$@"
