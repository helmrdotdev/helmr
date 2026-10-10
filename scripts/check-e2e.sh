#!/usr/bin/env bash
# Offline checks for behavior cases; does not deploy or execute a real workload.
set -euo pipefail
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
node scripts/check-node-toolchain.mjs
node --test scripts/node-toolchain.test.mjs
if [ "$#" -eq 0 ]; then
  scripts/build-npm-packages.sh
elif [ "$#" -ne 1 ] || [ "$1" != --skip-sdk-build ]; then
  echo "usage: $0 [--skip-sdk-build]" >&2
  exit 2
fi
for project in tests/e2e tests/e2e/fixtures/schedule; do
  (cd "$project" && bun install --frozen-lockfile --ignore-scripts && bun run typecheck)
done
bun test ./tests/e2e
