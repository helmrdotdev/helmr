#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
fixture="${tmp}/repo"
mkdir -p "${fixture}/bin"

git -C "${fixture}" init --quiet
printf 'a\n' >"${fixture}/input.txt"
git -C "${fixture}" add input.txt
printf '#!/bin/sh\nexit 0\n' >"${fixture}/bin/guestd"
chmod 0755 "${fixture}/bin/guestd"

run_guestd_check() {
  make -s -f "${repo_root}/images/boot-artifacts.mk" guestd \
    REPO_ROOT="${fixture}" \
    GUESTD="${fixture}/bin/guestd" \
    GUESTD_INPUT_PATHS=input.txt \
    HELMR_GUESTD_BUILT=1
}

stamp="${fixture}/bin/.guestd-inputs.x86_64.sha256"
run_guestd_check
stamp_a="$(cat "${stamp}")"
source_hash_a="${stamp_a%% *}"

printf '#!/bin/sh\nexit 1\n' >"${fixture}/bin/guestd"
chmod 0755 "${fixture}/bin/guestd"
run_guestd_check
stamp_binary_changed="$(cat "${stamp}")"
if [ "${stamp_binary_changed}" = "${stamp_a}" ] ||
  [ "${stamp_binary_changed%% *}" != "${source_hash_a}" ]; then
  printf 'guestd byte change did not update only the binary digest\n' >&2
  exit 1
fi

printf 'b\n' >"${fixture}/input.txt"
if make -s -f "${repo_root}/images/boot-artifacts.mk" guestd-stamp-current \
  REPO_ROOT="${fixture}" \
  GUESTD="${fixture}/bin/guestd" \
  GUESTD_INPUT_PATHS=input.txt >/dev/null 2>&1; then
  printf 'stale guestd input stamp was accepted\n' >&2
  exit 1
fi
run_guestd_check
stamp_b="$(cat "${stamp}")"
if [ "${source_hash_a}" = "${stamp_b%% *}" ]; then
  printf 'guestd input stamp did not change for new content\n' >&2
  exit 1
fi

touch -d @1 "${stamp}"
printf 'a\n' >"${fixture}/input.txt"
run_guestd_check
if [ "${source_hash_a}" != "$(cut -d ' ' -f 1 "${stamp}")" ]; then
  printf 'guestd input stamp did not return to the original content hash\n' >&2
  exit 1
fi
if [ "$(stat -c %Y "${stamp}")" -le 1 ]; then
  printf 'guestd input stamp was not refreshed on input rollback\n' >&2
  exit 1
fi

printf 'ok - boot artifact Make tests\n'
