#!/usr/bin/env bash
#
# ao-here.sh — register the current (or given) directory as an AO project and start the daemon.
#
# Usage:
#   ./ao-here.sh                 # uses $PWD
#   ./ao-here.sh /path/to/repo   # uses given path
#
# Env overrides:
#   AO_HOST (default 127.0.0.1)
#   AO_PORT (default 3001)

set -euo pipefail

PROJECT_PATH="$(cd "${1:-$PWD}" && pwd)"

if [[ ! -d "$PROJECT_PATH/.git" ]]; then
  echo "error: $PROJECT_PATH is not a git repository (no .git dir)" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "error: 'jq' is required (brew install jq)" >&2
  exit 1
fi

AO_HOST="${AO_HOST:-127.0.0.1}"
AO_PORT="${AO_PORT:-3001}"
BASE="http://${AO_HOST}:${AO_PORT}"

is_ready() { curl -fsS --max-time 1 "${BASE}/readyz" >/dev/null 2>&1; }

if is_ready; then
  echo "[ao] daemon already running at ${BASE}"
else
  if ! command -v ao >/dev/null 2>&1; then
    echo "error: 'ao' not on PATH. Build it: cd <agent-orchestrator>/backend && go install ./cmd/ao" >&2
    exit 1
  fi
  echo "[ao] starting daemon..."
  ao start
  for _ in {1..30}; do
    if is_ready; then break; fi
    sleep 1
  done
  if ! is_ready; then
    echo "error: daemon did not become ready in 30s at ${BASE}" >&2
    exit 1
  fi
  echo "[ao] daemon ready at ${BASE}"
fi

BODY="$(jq -nc --arg path "$PROJECT_PATH" '{path: $path}')"
RESPONSE="$(curl -sS -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' -d "$BODY" "${BASE}/api/v1/projects")"
HTTP_CODE="$(echo "$RESPONSE" | tail -1)"
BODY_OUT="$(echo "$RESPONSE" | sed '$d')"

case "$HTTP_CODE" in
  201)
    PROJECT_ID="$(echo "$BODY_OUT" | jq -r '.project.id')"
    echo "[ao] registered project: $PROJECT_ID  ->  $PROJECT_PATH"
    ;;
  409)
    PROJECT_ID="$(echo "$BODY_OUT" | jq -r '.error.details.existingProjectId // empty')"
    if [[ -z "$PROJECT_ID" ]]; then
      echo "error: conflict response missing existingProjectId; raw:" >&2
      echo "$BODY_OUT" | jq . >&2 2>/dev/null || echo "$BODY_OUT" >&2
      exit 1
    fi
    echo "[ao] project already registered: $PROJECT_ID  ->  $PROJECT_PATH"
    ;;
  *)
    echo "error: unexpected HTTP $HTTP_CODE from POST /api/v1/projects:" >&2
    echo "$BODY_OUT" | jq . >&2 2>/dev/null || echo "$BODY_OUT" >&2
    exit 1
    ;;
esac

echo ""
echo "  next: ao spawn --project $PROJECT_ID --prompt \"<your task>\""
