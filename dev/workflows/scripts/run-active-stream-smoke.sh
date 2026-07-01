#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

cd "${ROOT}"
SMOKE_CASES=active-stream "${ROOT}/dev/workflows/scripts/run-release-smoke.sh"
