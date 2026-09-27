#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
node scripts/check-node-toolchain.mjs
node --test tests/node_toolchain_test.mjs
scripts/build-npm-packages.sh
for project in tests/e2e tests/e2e/fixtures/schedule examples/issue-fixer; do
  (cd "$project" && bun install --frozen-lockfile --ignore-scripts && bun run typecheck)
done
bun test tests/runtime_cases tests/runtime_actor tests/e2e/cases/runtime/payload.test.mjs examples/issue-fixer/tests
scripts/check-packed-sdk-consumer.sh
scripts/build-compiler-entry.sh
scripts/build-hostconfig-entry.sh
node scripts/check-dev-samples.mjs
