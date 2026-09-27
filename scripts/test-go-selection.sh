#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 2 ]; then
  printf 'usage: %s TOP_LEVEL_TEST_PATTERN PACKAGE...\n' "$0" >&2
  exit 2
fi

pattern=$1
shift
# Go uses separately anchored regular expressions for slash-separated subtests.
# This entrypoint proves top-level cases; run the containing case for subtests.
if [[ "$pattern" == */* ]]; then
  echo 'select a top-level Go test pattern; subtest selectors are not supported' >&2
  exit 2
fi
packages="$(go list -f '{{.ImportPath}}' "$@")"
result="$(mktemp)"
trap 'rm -f "${result}"' EXIT

if ! go test -json -run "${pattern}" -count=1 "$@" >"${result}"; then
  jq -r 'select(.Action == "output") | .Output' "${result}" |
    tail -n 200 >&2
  exit 1
fi

# A green process exit can hide skipped prerequisites. A selected verification
# needs executed assertions, not a package with zero tests or a skipped case.
if jq -se 'any(.[]; .Action == "skip" and (.Test? | type) == "string")' "$result" >/dev/null; then
  jq -r 'select(.Action == "skip" and (.Test? | type) == "string") | "selected test skipped: \(.Package)/\(.Test)"' "$result" >&2
  exit 1
fi

matched=true
while IFS= read -r package; do
  if jq -se --arg package "${package}" '
    any(.[];
      .Package == $package and
      .Action == "pass" and
      (.Test? | type) == "string" and
      (.Test | contains("/") | not)
    )
  ' "${result}" >/dev/null; then
    continue
  fi
  printf 'no Go test matched %q in %s\n' "${pattern}" "${package}" >&2
  matched=false
done <<<"${packages}"

[ "${matched}" = true ]
