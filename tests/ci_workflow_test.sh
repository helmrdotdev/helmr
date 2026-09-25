#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/ci.yaml"
cd "$repo_root"

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
require_text 'python3 scripts/release/ci_policy.py source' \
  "source aggregate does not verify selected dependencies"
require_text 'python3 scripts/release/ci_policy.py pr' \
  "PR aggregate does not verify selected dependencies"
require_text 'CI_NEEDS: ${{ toJSON(needs) }}' \
  "aggregates must use actual native job results"
require_text 'types: [opened, synchronize, reopened, labeled, unlabeled]' \
  "full-check label changes must rerun current PR checks"
require_text 'needs: artifact-selection' \
  "artifact build does not depend on source selection"
require_text 'selection: ${{ needs.artifact-selection.outputs.selection }}' \
  "artifact build does not use the selected exact source"
require_text 'skip_artifacts' \
  "artifact selection does not expose documentation-only skip"
require_text 'fetch-depth: 0' \
  "selection needs the complete main/PR comparison history"
require_text 'run_bundle_builder' \
  "builder selection output is missing"
require_text 'name: source-ci-complete' \
  "required source aggregate missing on main"
require_text 'name: preview-ready' \
  "preview readiness is not separated from source CI on main"
require_text 'github.event_name == '\''pull_request'\''' \
  "pull-request aggregate no longer scoped to PR events"
python3 - <<'PYTHON'
from pathlib import Path

root = Path.cwd()
workflow = (root / '.github/workflows/ci.yaml').read_text()
source = workflow.split('  source-ci-complete:', 1)[1].split('  preview-ready:', 1)[0]
pr = workflow.split('  ci-complete:', 1)[1]
assert '      - artifact-selection' in source and '      - artifact-selection' in pr
assert '      - source-ci-complete' in pr and '      - build-artifacts' in pr
assert "if: github.event_name == 'push' && needs.artifact-selection.outputs.skip_artifacts == 'true'" in workflow
assert "if: needs.artifact-selection.outputs.skip_artifacts == 'false'" in workflow
# Selected jobs must be gated at job level, not return a green skipped command.
for job in ('nix-flake', 'postgres', 'browser', 'release-contracts'):
    block = workflow.split('  '+job+':\n', 1)[1]
    expected = "    needs: artifact-selection\n    if: contains(fromJSON(needs.artifact-selection.outputs.source_checks), '" + job + "')\n"
    assert block.startswith(expected), job + ' must use the source selection'
assert 'matrix: ${{ fromJSON(needs.artifact-selection.outputs.repo_matrix) }}' in workflow
assert "checks = classify([])" in workflow, 'main must default to complete source checks'
assert "output.write('source_checks='+json.dumps(checks['source_checks'])" in workflow
assert "output.write('repo_matrix='+json.dumps(repo_matrix(checks['source_checks']))" in workflow
build = (root / '.github/workflows/build-artifacts.yaml').read_text()
retention = "retention-days: ${{ github.event_name == 'pull_request' && 1 || 7 }}"
assert build.count(retention) == 2, 'both frozen PR component and CLI uploads need short retention'
release = (root / '.github/workflows/release.yaml').read_text()
assert 'retention-days: 7' in release, 'release readback retry window must remain intact'
assert 'if: always()' in source and 'if: always()' in pr
PYTHON

printf 'ok - CI workflow policy\n'
