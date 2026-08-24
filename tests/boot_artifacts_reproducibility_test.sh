#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

if ! git -C "${repo_root}" diff --quiet ||
  ! git -C "${repo_root}" diff --cached --quiet ||
  [ -n "$(git -C "${repo_root}" ls-files --others --exclude-standard)" ]; then
  printf 'boot artifact reproducibility test requires a clean checkout\n' >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
clone_a="${tmp}/checkout-a"
clone_b="${tmp}/checkout-b"

git clone --quiet --no-local --depth 1 "${repo_root}" "${clone_a}"
git clone --quiet --no-local --depth 1 "${repo_root}" "${clone_b}"

printf 'unrelated source change\n' >"${clone_b}/boot-artifact-repro-sentinel.txt"
git -C "${clone_b}" add boot-artifact-repro-sentinel.txt
git -C "${clone_b}" \
  -c user.name='Helmr Reproducibility Test' \
  -c user.email='reproducibility-test@helmr.invalid' \
  commit --quiet -m 'add unrelated reproducibility sentinel'

for checkout in "${clone_a}" "${clone_b}"; do
  (
    cd "${checkout}"
    nix run .#ci-boot-artifacts
    ./scripts/materialize-worker-runtime-bundle.sh \
      "${checkout}/dist/runtime-bundle" \
      "${checkout}/images/guest/out" >/dev/null
  )
  if ! git -C "${checkout}" diff --quiet ||
    ! git -C "${checkout}" diff --cached --quiet ||
    [ -n "$(git -C "${checkout}" ls-files --others --exclude-standard)" ]; then
    printf 'boot artifact build modified source checkout: %s\n' "${checkout}" >&2
    exit 1
  fi
done

for name in vmlinuz initramfs rootfs.squashfs runtime-artifacts.json; do
  cmp \
    "${clone_a}/images/guest/out/${name}" \
    "${clone_b}/images/guest/out/${name}"
done
cmp \
  "${clone_a}/dist/runtime-bundle/runtime-artifacts.tar" \
  "${clone_b}/dist/runtime-bundle/runtime-artifacts.tar"

for checkout in "${clone_a}" "${clone_b}"; do
  commit="$(git -C "${checkout}" rev-parse HEAD)"
  jq -e --arg commit "${commit}" '.sourceCommit == $commit' \
    "${checkout}/dist/runtime-bundle/worker-runtime-bundle.json" >/dev/null
done
jq -S 'del(.sourceCommit)' \
  "${clone_a}/dist/runtime-bundle/worker-runtime-bundle.json" \
  >"${tmp}/receipt-a.json"
jq -S 'del(.sourceCommit)' \
  "${clone_b}/dist/runtime-bundle/worker-runtime-bundle.json" \
  >"${tmp}/receipt-b.json"
cmp "${tmp}/receipt-a.json" "${tmp}/receipt-b.json"

mkdir -p "${repo_root}/dist"
for name in guest-vmlinuz guest-initramfs guest-rootfs.squashfs; do
  install -m 0644 "${clone_a}/dist/${name}" "${repo_root}/dist/${name}"
done

printf 'ok - boot artifact reproducibility tests\n'
