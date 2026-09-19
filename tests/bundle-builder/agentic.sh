#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=tests/bundle-builder/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"

# Agent tool work: the real SDK declares the tasks and a Workspace image with a
# browser, Git and Python; the Program carries Playwright and Sharp.
agentic_project="$tmp/agentic-project"
cp -a "$repo_root/tests/fixtures/agentic-work" "$agentic_project"
prepare_host_sdk "$agentic_project"
"$shared/helmr" build "$agentic_project" --output "$tmp/agentic" 2>"$tmp/agentic.log" ||
  { tail -n 120 "$tmp/agentic.log" >&2; exit 1; }
jq -e '[.workspaceImages[].declaredId] == ["agentic-work"]' "$tmp/agentic/bundle.json" >/dev/null
HELMR_AGENTIC_BUNDLE="$tmp/agentic" bash "$repo_root/tests/agentic_work_e2e.sh"

printf 'ok - bundle builder agentic lane\n'
