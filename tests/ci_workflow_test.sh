#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PYTHON'
from pathlib import Path
text = Path('.github/workflows/ci.yaml').read_text()
assert 'check: [policy, go, typescript]' in text
assert 'if: always()' in text and 'needs: checks' in text
assert 'CHECKS_RESULT: ${{ needs.checks.result }}' in text
assert 'test "$CHECKS_RESULT" = success' in text
for obsolete in ('build-artifacts', 'artifact-selection', 'ci:full', 'fetch-depth', 'fromJSON', 'labeled', 'preview-ready'):
    assert obsolete not in text, obsolete
assert 'cancel-in-progress: true' in text
assert 'name: ci complete' in text
PYTHON
printf 'ok - fixed source CI and failing aggregate\n'
