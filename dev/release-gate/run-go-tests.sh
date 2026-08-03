#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 2 ]; then
  printf 'usage: %s TEST_PATTERN PACKAGE...\n' "$0" >&2
  exit 2
fi

pattern=$1
shift
packages="$(go list -f '{{.ImportPath}}' "$@")"
result="$(mktemp)"
trap 'rm -f "${result}"' EXIT

if ! go test -json -run "${pattern}" -count=1 "$@" >"${result}"; then
  jq -r 'select(.Action == "output") | .Output' "${result}" |
    tail -n 200 >&2
  exit 1
fi

matched=true
while IFS= read -r package; do
  if jq -se --arg pattern "${pattern}" --arg package "${package}" '
    any(.[];
      .Package == $package and
      .Action == "pass" and
      (.Test? | type) == "string" and
      (.Test | test($pattern))
    )
  ' "${result}" >/dev/null; then
    continue
  fi
  printf 'no Go test matched %q in %s\n' "${pattern}" "${package}" >&2
  matched=false
done <<<"${packages}"

[ "${matched}" = true ]
