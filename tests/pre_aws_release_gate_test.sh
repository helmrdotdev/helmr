#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
runner="${repo_root}/dev/release-gate/run-go-tests.sh"

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

printf 'ok - pre-AWS release gate tests\n'
