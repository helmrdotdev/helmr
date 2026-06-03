#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

bun install --frozen-lockfile --ignore-scripts
scripts/build-embedded-adapter.sh
git diff --exit-code -- internal/adapter/js
test -z "$(git status --porcelain -- internal/adapter/js)"
bun audit
scripts/security-checks.sh
bash tests/install_test.sh
bash tests/release_manifest_test.sh
bash tests/release_workflow_test.sh
bash tests/release_worker_image_identity_test.sh
bun run typecheck
bun run test:ts
make verify
make test-linux-compile
git diff --exit-code
