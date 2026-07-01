#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

bun install --frozen-lockfile --ignore-scripts
scripts/build-embedded-adapter.sh
git diff --exit-code -- internal/adapter/js
test -z "$(git status --porcelain -- internal/adapter/js)"
bun audit
actionlint
scripts/security-checks.sh
bash tests/install_test.sh
bash tests/release_manifest_test.sh
bash tests/release_workflow_test.sh
bash tests/db_query_service_file_test.sh
bash tests/run_path_report_local_test.sh
bash tests/path_report_wrapper_test.sh
bash tests/surface_attestation_test.sh
bash tests/measurement_report_summary_test.sh
bash tests/release_smoke_selector_test.sh
bash tests/measurement_preflight_guard_test.sh
bash tests/release_worker_ami_cleanup_test.sh
bash tests/release_worker_image_identity_test.sh
bun run typecheck
bun run test:ts
make verify
make test-race
make test-linux-compile
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 staticcheck -tags embed_console ./...
git diff --exit-code
