#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

bun install --frozen-lockfile --ignore-scripts
scripts/check-dev-samples.sh
scripts/build-runtime-entry.sh --check
bun audit
actionlint
scripts/security-checks.sh
bash -n scripts/dev-console-stack.sh
bash tests/install_test.sh
bash tests/release_manifest_test.sh
bash tests/release_workflow_test.sh
bash tests/aws_release_artifacts_test.sh
bash tests/worker_host_bundle_test.sh
bash tests/worker_runtime_bundle_test.sh
bash tests/aws_bootstrap_helmr_secrets_test.sh
bash tests/release_smoke_selector_test.sh
bash tests/pre_aws_release_gate_test.sh
bash tests/release_worker_ami_cleanup_test.sh
bash tests/release_worker_image_identity_test.sh
bun run typecheck
bun run test:ts
make verify
make test-race
make test-linux-compile
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 staticcheck -tags embed_console ./...
git diff --exit-code
