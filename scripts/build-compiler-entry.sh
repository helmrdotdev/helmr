#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build-module-execution-entry.sh "$@"
bun scripts/build-platform-entries.ts compiler "$@"
