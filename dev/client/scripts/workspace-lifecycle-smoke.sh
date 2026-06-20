#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HELMR_API_URL="${HELMR_API_URL:-https://dev.helmr.dev}"
export HELMR_API_URL

if [ -z "${HELMR_API_KEY:-}" ]; then
  cat >&2 <<'MSG'
HELMR_API_KEY is required. Use an environment-scoped dev API key so the SDK
client exercises the same workspace API surface that deployed clients use.
MSG
  exit 2
fi

cd "${ROOT}"

if [ "${SKIP_DEPLOY:-0}" != "1" ]; then
  dev/workflows/scripts/sync-local-sdk.sh
  HELMR_API_URL="${HELMR_API_URL}" go run ./cmd/helmr deploy ./dev/workflows --timeout 20m
fi

HELMR_API_URL="${HELMR_API_URL}" bun run --cwd dev/client workspace:lifecycle
