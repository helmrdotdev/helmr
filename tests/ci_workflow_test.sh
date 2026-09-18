#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/ci.yaml"

require_text() {
  if ! rg -F -- "$1" "$workflow" >/dev/null; then
    printf 'not ok - %s\n' "$2" >&2
    exit 1
  fi
}

require_text 'group: ${{ github.workflow }}-${{ github.event_name == '\''pull_request'\'' && github.ref || github.run_id }}' \
  "main push runs do not have unique concurrency groups"
require_text 'cancel-in-progress: ${{ github.event_name == '\''pull_request'\'' }}' \
  "concurrency cancellation is not limited to pull requests"
require_text 'if [ "$result" != "success" ]; then' \
  "ci complete does not reject failed dependencies"
require_text 'exit 1' \
  "ci complete does not fail after a dependency failure"
require_text 'needs: artifact-selection' \
  "artifact build does not depend on source selection"
require_text 'selection: ${{ needs.artifact-selection.outputs.selection }}' \
  "artifact build does not use the selected exact source"
require_text 'skip_artifacts' \
  "artifact selection does not expose documentation-only skip"
require_text 'fetch-depth: ${{ github.event_name == '\''push'\'' && '\''0'\'' || '\'''\'' }}' \
  "artifact-selection must use full history only on main push"
require_text 'name: source-ci-complete' \
  "required source aggregate missing on main"
require_text 'name: preview-ready' \
  "preview readiness is not separated from source CI on main"
require_text 'github.event_name == '\''pull_request'\''' \
  "pull-request aggregate no longer scoped to PR events"
require_text '"${{ needs.source-ci-complete.result }}"' \
  "PR ci complete does not require source aggregate"
require_text '"${{ needs.build-artifacts.result }}"' \
  "PR ci complete does not require artifact build"

printf 'ok - CI workflow policy\n'
