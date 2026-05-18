#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

bun install --frozen-lockfile --ignore-scripts
bun audit
scripts/security-checks.sh
bash tests/install_test.sh
bash tests/release_manifest_test.sh
bun run typecheck
bun run test:ts
make verify
make test-linux-compile
git diff --exit-code
