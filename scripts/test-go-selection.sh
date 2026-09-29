#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s [-v] TOP_LEVEL_TEST_PATTERN PACKAGE...\n' "$0" >&2
  exit 2
}

# -v streams the selected tests' output to stderr as it happens, for runs whose
# logs are the evidence (measurements, Nix build logs). Without it, only a
# failing run replays its output.
verbose=false
if [ "${1:-}" = -v ]; then
  verbose=true
  shift
fi
if [ "$#" -lt 2 ]; then
  usage
fi

pattern=$1
shift
# Go uses separately anchored regular expressions for slash-separated subtests.
# This entrypoint proves top-level cases; run the containing case for subtests.
if [[ "$pattern" == */* ]]; then
  echo 'select a top-level Go test pattern; subtest selectors are not supported' >&2
  exit 2
fi
# An exact-name pattern (^Name$, or a fully grouped ^(A|B)$) names every test it
# must prove, so a renamed or moved member of an alternation cannot hide behind
# the others. Names are Go identifiers, which may be Unicode; jq's regex engine
# matches them independently of the shell locale. ^A|B$ is not exact: Go reads
# it as (^A)|(B$), so it keeps only the per-package check below.
exact_names=$(jq -nr --arg pattern "${pattern}" '
  def ident: "[\\p{L}_][\\p{L}\\p{Nd}_]*";
  $pattern
  | (capture("^\\^(?<names>" + ident + ")\\$$")
     // capture("^\\^\\((?<names>" + ident + "(?:\\|" + ident + ")*)\\)\\$$")
     // empty)
  | .names
  | split("|")[]
')
required_names=()
if [ -n "${exact_names}" ]; then
  while IFS= read -r name; do
    required_names+=("${name}")
  done <<<"${exact_names}"
fi
packages="$(go list -f '{{.ImportPath}}' "$@")"
result="$(mktemp)"
trap 'rm -f "${result}"' EXIT

output_events='select(.Action == "output" or .Action == "build-output") | .Output'
if [ "${verbose}" = true ]; then
  if ! go test -json -run "${pattern}" -count=1 "$@" |
    tee "${result}" |
    jq --unbuffered -j "${output_events}" >&2; then
    exit 1
  fi
elif ! go test -json -run "${pattern}" -count=1 "$@" >"${result}"; then
  jq -j "${output_events}" "${result}" >&2
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

for name in ${required_names[@]+"${required_names[@]}"}; do
  if ! jq -se --arg name "${name}" 'any(.[]; .Action == "pass" and .Test == $name)' "${result}" >/dev/null; then
    printf 'selected Go test did not pass: %s\n' "${name}" >&2
    matched=false
  fi
done

[ "${matched}" = true ]

# Keep successful package evidence visible when this runner is redirected to a log.
jq -r 'select(.Action == "pass" and .Test == null) | "PASS \(.Package) (\(.Elapsed)s)"' "$result"
