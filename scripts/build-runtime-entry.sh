#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build-module-loader-entry.sh "$@"
bun scripts/build-platform-entries.ts runtime "$@"
