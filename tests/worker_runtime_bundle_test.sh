#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
artifacts="${tmp}/artifacts"
mkdir -p "${artifacts}"

printf 'kernel-bytes' >"${artifacts}/vmlinuz"
printf 'initramfs-bytes' >"${artifacts}/initramfs"
printf 'rootfs-bytes' >"${artifacts}/rootfs.squashfs"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

jq -cn \
  --arg kernel_digest "sha256:$(sha256_file "${artifacts}/vmlinuz")" \
  --argjson kernel_size "$(wc -c <"${artifacts}/vmlinuz" | tr -d ' ')" \
  --arg initramfs_digest "sha256:$(sha256_file "${artifacts}/initramfs")" \
  --argjson initramfs_size "$(wc -c <"${artifacts}/initramfs" | tr -d ' ')" \
  --arg rootfs_digest "sha256:$(sha256_file "${artifacts}/rootfs.squashfs")" \
  --argjson rootfs_size "$(wc -c <"${artifacts}/rootfs.squashfs" | tr -d ' ')" '
  {
    schema: "helmr.runtime-artifacts.v0",
    arch: "amd64",
    vm_runtime_contract: "helmr.vm-runtime.v0",
    kernel: {path: "vmlinuz", digest: $kernel_digest, size_bytes: $kernel_size},
    initramfs: {path: "initramfs", digest: $initramfs_digest, size_bytes: $initramfs_size},
    rootfs: {path: "rootfs.squashfs", digest: $rootfs_digest, size_bytes: $rootfs_size}
  }
' >"${artifacts}/runtime-artifacts.json"

"${repo_root}/scripts/materialize-worker-runtime-bundle.sh" "${tmp}/bundle-a" "${artifacts}" >/dev/null
"${repo_root}/scripts/materialize-worker-runtime-bundle.sh" "${tmp}/bundle-b" "${artifacts}" >/dev/null

cmp "${tmp}/bundle-a/runtime-artifacts.tar" "${tmp}/bundle-b/runtime-artifacts.tar"
jq -e '
  (keys | sort) == ["bundle", "runtimeArtifactsManifest", "schema", "sourceCommit"] and
  .schema == "helmr.worker-runtime-bundle.v0" and
  (.sourceCommit | test("^[0-9a-f]{40}$")) and
  .bundle.path == "runtime-artifacts.tar" and
  (.bundle.digest | test("^sha256:[0-9a-f]{64}$")) and
  .runtimeArtifactsManifest.path == "runtime-artifacts.json" and
  (.runtimeArtifactsManifest.digest | test("^sha256:[0-9a-f]{64}$"))
' "${tmp}/bundle-a/worker-runtime-bundle.json" >/dev/null

expected_entries="$(printf '%s\n' initramfs rootfs.squashfs runtime-artifacts.json vmlinuz)"
[ "$(tar -tf "${tmp}/bundle-a/runtime-artifacts.tar")" = "${expected_entries}" ]
mkdir "${tmp}/unpacked"
tar -C "${tmp}/unpacked" -xf "${tmp}/bundle-a/runtime-artifacts.tar"
for name in vmlinuz initramfs rootfs.squashfs runtime-artifacts.json; do
  cmp "${artifacts}/${name}" "${tmp}/unpacked/${name}"
done

printf 'ok - Worker runtime bundle tests\n'
