#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
runner="${repo_root}/dev/release-gate/run-go-tests.sh"
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

printf 'ok - pre-AWS release gate tests\n'
