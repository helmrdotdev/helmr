#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

dev/workflows/scripts/sync-local-sdk.sh
bun run --cwd dev/workflows typecheck
bun run --cwd dev/schedule-workflows typecheck
bun run --cwd dev/client typecheck
scripts/check-packed-sdk-consumer.sh
scripts/build-compiler-entry.sh
node scripts/check-dev-samples.mjs
