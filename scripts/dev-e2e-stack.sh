#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export HELMR_DEV_DIR="${HELMR_DEV_DIR:-${ROOT}/.helmr-dev-e2e}"
export HELMR_DEV_CONSOLE_MODE="${HELMR_DEV_CONSOLE_MODE:-preview}"

"${ROOT}/scripts/dev-reset.sh"
exec "${ROOT}/scripts/dev-console-stack.sh"
