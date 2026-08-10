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

if [ "$role" = "guest" ]; then
	go_buildkit_version="$(go list -m -f '{{.Version}}' github.com/moby/buildkit)"
	guest_buildkit_version="$(
		jq -er '
			[.contents.packages[] | select(.name == "buildkitd") | .version]
			| if length == 1 then .[0] else error("expected exactly one buildkitd package") end
		' "images/${role}/apko.${arch}.lock.json"
	)"
	guest_buildkit_upstream="${guest_buildkit_version%-r*}"
	guest_buildkit_revision="${guest_buildkit_version##*-r}"
	case "$guest_buildkit_revision" in
		''|*[!0-9]*)
			echo "guest BuildKit package revision is invalid: $guest_buildkit_version" >&2
			exit 1
			;;
	esac
	if [ "$guest_buildkit_upstream" != "${go_buildkit_version#v}" ]; then
		echo "guest BuildKit $guest_buildkit_version does not match Go module $go_buildkit_version" >&2
		exit 1
	fi

	if ! unsquashfs -lln "images/${role}/out/rootfs.squashfs" |
		awk '$1 == "-rwxr-xr-x" && $6 == "squashfs-root/usr/bin/buildkitd" { found = 1 } END { exit !found }'; then
		echo "guest rootfs does not contain an executable /usr/bin/buildkitd" >&2
		exit 1
	fi
fi

mkdir -p dist
cp "images/${role}/out/vmlinuz" "dist/${prefix}-vmlinuz"
cp "images/${role}/out/initramfs" "dist/${prefix}-initramfs"
cp "images/${role}/out/rootfs.squashfs" "dist/${prefix}-rootfs.squashfs"

sha256sum "dist/${prefix}-vmlinuz" "dist/${prefix}-initramfs" "dist/${prefix}-rootfs.squashfs"
ls -lh dist
