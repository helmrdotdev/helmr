#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
role=${ROLE:-guest}
arch=${ARCH:-aarch64}
prefix=${PREFIX:-$role}

cd "$repo_root"

bun install --frozen-lockfile --ignore-scripts
ARCH="$arch" ./scripts/build-guestd-linux.sh

mkdir -p out
apko build "images/${role}/apko.yaml" "helmr-${role}:ci" "out/${role}.oci.tar" \
	--arch "$arch" \
	--lockfile "images/${role}/apko.${arch}.lock.json"

ARCH="$arch" HELMR_GUESTD_BUILT=1 make -C "images/${role}" all

mkdir -p dist
cp "images/${role}/out/vmlinuz" "dist/${prefix}-vmlinuz"
cp "images/${role}/out/initramfs" "dist/${prefix}-initramfs"
cp "images/${role}/out/rootfs.ext4" "dist/${prefix}-rootfs.ext4"

sha256sum "dist/${prefix}-vmlinuz" "dist/${prefix}-initramfs" "dist/${prefix}-rootfs.ext4"
ls -lh dist
