#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
runner="${repo_root}/scripts/test-go-selection.sh"
gate="${repo_root}/dev/release-gate/check-pre-aws.sh"

found_script=false
while IFS= read -r script; do
  found_script=true
  if [ ! -f "${repo_root}/${script}" ]; then
    printf 'not ok - pre-AWS gate invokes missing Product script: %s\n' "${script}" >&2
    exit 1
  fi
done < <(
  LC_ALL=C grep -Eo '(dev|scripts|tests)/[[:alnum:]_./-]+\.sh' "${gate}" |
    LC_ALL=C sort -u
)

if [ "${found_script}" != true ]; then
  printf 'not ok - pre-AWS gate did not invoke any Product scripts\n' >&2
  exit 1
fi

"${runner}" '^TestParse$' "${repo_root}/internal/ids"
if "${runner}" '^TestDoesNotExist$' "${repo_root}/internal/ids" >/dev/null 2>&1; then
  printf 'not ok - missing Go test selection was accepted\n' >&2
  exit 1
fi

if "${runner}" '^TestParse$' \
  "${repo_root}/internal/ids" \
  "${repo_root}/internal/compute" >/dev/null 2>&1; then
  printf 'not ok - partial package match was accepted\n' >&2
  exit 1
fi

fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cat >"$fixture/go.mod" <<'EOF'
module selectedtest

go 1.27.1
EOF
cat >"$fixture/selection_test.go" <<'EOF'
package selectedtest
import "testing"
func TestReady(t *testing.T) {}
func TestUnavailable(t *testing.T) { t.Skip("required service unavailable") }
func TestNested(t *testing.T) { t.Run("needs-service", func(t *testing.T) { t.Skip("missing service") }) }
EOF
(
  cd "$fixture"
  "$runner" '^TestReady$' .
  for pattern in '^TestNested$/^needs-service$' '^TestUnavailable$' '^TestNested$' '^Test(Ready|Unavailable)$'; do
    if "$runner" "$pattern" . >/dev/null 2>&1; then
      echo "not ok - skipped selected assertion was accepted: $pattern" >&2
      exit 1
    fi
  done
)
printf 'ok - pre-AWS release gate tests\n'
