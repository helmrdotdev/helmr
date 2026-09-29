#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
selection="$repo_root/scripts/test-go-selection.sh"
cd "$repo_root"

# `go test -run` exits 0 when the selection matches nothing or skips, so a
# renamed, moved or gated test would silently stop proving its check. Pinned
# selections in automation go through the selection runner instead.
direct_selections() {
  # shellcheck disable=SC2016 # awk program text, not shell expansion.
  awk '
    FNR == 1 { joined = "" }
    joined == "" && /^[[:space:]]*#/ { next }
    /\\$/ { joined = joined substr($0, 1, length($0) - 1) " "; next }
    {
      line = joined $0
      joined = ""
      if (line ~ /(^|[^[:alnum:]_.-])(go|\$\(GO\)|"?\$\{?GO\}?"?)[[:space:]]+(-C[[:space:]]+[^[:space:]]+[[:space:]]+)?test[[:space:]]/ && line ~ /[[:space:]]-run([=[:space:]]|$)/) {
        print FILENAME ":" FNR ": " line
      }
    }
  ' "$@"
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/helmr-go-selection.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT

# shellcheck disable=SC2016 # literal fixture commands.
printf '%s\n' \
  "# go test ./commented -run '^TestComment\$'" \
  "go test ./plain -run '^TestPlain\$'" \
  '"$GO" test ./quoted -run=^TestQuoted$' \
  "\${GO} test ./braced \\" \
  "  -run '^TestBraced\$'" \
  '$(GO) test ./make -count=1 -run ^TestMake$' \
  'go -C "$root" test ./chdir -run ^TestChdir$' \
  'go test ./unpinned' \
  "bash scripts/test-go-selection.sh '^TestRouted\$' ./routed" \
  >"$tmp/guard-fixture.sh"
guard_hits=$(direct_selections "$tmp/guard-fixture.sh" | grep -oE '\./[a-z]+' | tr '\n' ' ')
if [ "$guard_hits" != './plain ./quoted ./braced ./make ./chdir ' ]; then
  printf 'guard matched unexpected selections: %s\n' "$guard_hits" >&2
  exit 1
fi

automation_files=()
while IFS= read -r -d '' file; do
  automation_files+=("$file")
done < <(find nix scripts tests .github Makefile -type f ! -name '*.md' \
  ! -path scripts/test-go-selection.sh ! -path tests/build/go-test-selection.test.sh -print0)
direct=$(direct_selections "${automation_files[@]}")
if [ -n "$direct" ]; then
  printf '%s\n' "$direct" >&2
  echo 'pinned Go test selections must use scripts/test-go-selection.sh so a skipped or unmatched test fails' >&2
  exit 1
fi
printf 'ok - pinned Go test selections use the selection runner\n'
mkdir -p "$tmp/fixture/other"
cat >"$tmp/fixture/go.mod" <<'EOF'
module example.test/selection

go 1.22
EOF
cat >"$tmp/fixture/fixture_test.go" <<'EOF'
package fixture

import (
	"os"
	"testing"
)

func TestPasses(t *testing.T) { t.Log("fixture evidence") }

func TestAlsoPasses(t *testing.T) {}

func TestÜber(t *testing.T) {}

func TestGated(t *testing.T) {
	if os.Getenv("FIXTURE_GATE") == "" {
		t.Skip("FIXTURE_GATE is not set")
	}
}

func TestFails(t *testing.T) {
	if os.Getenv("FIXTURE_FAIL") != "" {
		t.Fatal("fixture failure")
	}
}

func TestSkippedSubtest(t *testing.T) {
	t.Run("case", func(t *testing.T) { t.Skip("not configured") })
}
EOF
cat >"$tmp/fixture/other/other_test.go" <<'EOF'
package other

import "testing"

func TestOther(t *testing.T) {}
EOF

run() {
  (cd "$tmp/fixture" && GOFLAGS=-mod=mod GOWORK=off bash "$selection" "$@") >"$tmp/out" 2>&1
}
expect_pass() {
  if ! run "$@"; then
    cat "$tmp/out" >&2
    printf 'selection unexpectedly failed: %s\n' "$*" >&2
    exit 1
  fi
}
expect_fail() {
  local message=$1
  shift
  if run "$@"; then
    cat "$tmp/out" >&2
    printf 'selection unexpectedly passed: %s\n' "$*" >&2
    exit 1
  fi
  if ! grep -F -- "$message" "$tmp/out" >/dev/null; then
    cat "$tmp/out" >&2
    printf 'selection failed without %q: %s\n' "$message" "$*" >&2
    exit 1
  fi
}

expect_pass '^TestPasses$' .
expect_pass '^(TestPasses|TestAlsoPasses)$' .
FIXTURE_GATE=1 expect_pass '^TestGated$' .
expect_pass '^(TestPasses|TestOther)$' . ./other
expect_pass '^(TestPasses|TestÜber)$' .
expect_fail 'no Go test matched' '^TestRenamed$' .
FIXTURE_FAIL=1 expect_fail 'fixture failure' '^TestFails$' .
FIXTURE_FAIL=1 expect_fail 'fixture failure' -v '^TestFails$' .
expect_fail 'selected Go test did not pass: TestRenamed' '^(TestPasses|TestRenamed)$' .
expect_fail 'selected Go test did not pass: Test日本語' '^(TestPasses|Test日本語)$' .
# Go reads ^A|B$ as (^A)|(B$), a prefix or suffix match rather than a list of
# exact names, so the runner keeps only its per-package check for it.
expect_pass '^TestPasses|TestRenamed$' .
expect_fail 'selected test skipped' '^TestGated$' .
expect_fail 'selected test skipped' '^TestSkippedSubtest$' .
expect_fail 'no Go test matched' '^TestPasses$' . ./other
expect_fail 'subtest selectors are not supported' '^TestSkippedSubtest$/case' .
printf 'ok - selection runner rejects unmatched, partially matched and skipped tests\n'

expect_pass '^TestPasses$' .
if grep -F 'fixture evidence' "$tmp/out" >/dev/null; then
  echo 'selection replayed test output without -v' >&2
  exit 1
fi
expect_pass -v '^TestPasses$' .
grep -F 'fixture evidence' "$tmp/out" >/dev/null
printf 'ok - selection runner streams passing test output only with -v\n'
