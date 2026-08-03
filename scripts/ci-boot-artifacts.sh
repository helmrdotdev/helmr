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
bun install --frozen-lockfile --ignore-scripts
ARCH="$arch" ./scripts/build-guestd-linux.sh

mkdir -p out
apko build "images/${role}/apko.yaml" "helmr-${role}:ci" "out/${role}.oci.tar" \
	--arch "$arch" \
	--lockfile "images/${role}/apko.${arch}.lock.json"

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

	guest_buildkit_binary="$(mktemp)"
	trap 'rm -f "$guest_buildkit_binary"' EXIT
	debugfs -R "dump -p /usr/bin/buildkitd $guest_buildkit_binary" \
		"images/${role}/out/rootfs.ext4" >/dev/null 2>&1
	if [ ! -s "$guest_buildkit_binary" ] || [ ! -x "$guest_buildkit_binary" ]; then
		echo "guest rootfs does not contain an executable /usr/bin/buildkitd" >&2
		exit 1
	fi
	rm -f "$guest_buildkit_binary"
	trap - EXIT
fi

mkdir -p dist
cp "images/${role}/out/vmlinuz" "dist/${prefix}-vmlinuz"
cp "images/${role}/out/initramfs" "dist/${prefix}-initramfs"
cp "images/${role}/out/rootfs.ext4" "dist/${prefix}-rootfs.ext4"

sha256sum "dist/${prefix}-vmlinuz" "dist/${prefix}-initramfs" "dist/${prefix}-rootfs.ext4"
ls -lh dist
