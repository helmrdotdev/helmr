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

printf 'ok - CI workflow policy\n'
