#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
role=${ROLE:-guest}
arch=${ARCH:-x86_64}
prefix=${PREFIX:-$role}

if [ "$arch" != "x86_64" ]; then
	echo "unsupported ARCH: $arch" >&2
	exit 1
fi

cd "$repo_root"

./scripts/check-apko-lock.sh
ARCH="$arch" ./scripts/build-guestd-linux.sh

ARCH="$arch" HELMR_GUESTD_BUILT=1 make -C "images/${role}" all

mkdir -p dist
cp "images/${role}/out/vmlinuz" "dist/${prefix}-vmlinuz"
cp "images/${role}/out/initramfs" "dist/${prefix}-initramfs"
cp "images/${role}/out/rootfs.squashfs" "dist/${prefix}-rootfs.squashfs"

sha256sum "dist/${prefix}-vmlinuz" "dist/${prefix}-initramfs" "dist/${prefix}-rootfs.squashfs"
ls -lh dist
