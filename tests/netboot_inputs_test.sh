#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

source_dir="${tmp}/source"
out="${tmp}/out"
mkdir -p "${source_dir}" "${out}"

for name in initramfs-virt modloop-virt vmlinuz-virt; do
  printf 'fixture-%s\n' "${name}" >"${source_dir}/${name}"
done
(
  cd "${source_dir}"
  sha256sum initramfs-virt modloop-virt vmlinuz-virt
) >"${tmp}/checksums"

"${repo_root}/images/fetch-netboot.sh" \
  "${tmp}/checksums" \
  "file://${source_dir}" \
  "${out}"

for name in initramfs-virt modloop-virt vmlinuz-virt; do
  cmp "${source_dir}/${name}" "${out}/${name}"
done

printf 'corrupt-cache\n' >"${out}/modloop-virt"
"${repo_root}/images/fetch-netboot.sh" \
  "${tmp}/checksums" \
  "file://${source_dir}" \
  "${out}"
cmp "${source_dir}/modloop-virt" "${out}/modloop-virt"

bad_out="${tmp}/bad-out"
sed 's/^[0-9a-f]\{64\}/0000000000000000000000000000000000000000000000000000000000000000/' \
  "${tmp}/checksums" >"${tmp}/bad-checksums"
if "${repo_root}/images/fetch-netboot.sh" \
  "${tmp}/bad-checksums" \
  "file://${source_dir}" \
  "${bad_out}" >/dev/null 2>&1; then
  printf 'netboot fetch unexpectedly accepted a digest mismatch\n' >&2
  exit 1
fi
if find "${bad_out}" -type f -print -quit | grep -q .; then
  printf 'netboot fetch retained an artifact after digest failure\n' >&2
  exit 1
fi

cp "${tmp}/checksums" "${tmp}/extra-checksums"
printf '%064d  extra-file\n' 0 >>"${tmp}/extra-checksums"
if "${repo_root}/images/fetch-netboot.sh" \
  "${tmp}/extra-checksums" \
  "file://${source_dir}" \
  "${tmp}/extra-out" >/dev/null 2>&1; then
  printf 'netboot fetch unexpectedly accepted an extra checksum entry\n' >&2
  exit 1
fi

printf 'ok - netboot input tests\n'
