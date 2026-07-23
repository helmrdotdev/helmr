#!/bin/sh
set -eu

role=$1
repo_root=$2
role_dir=$3
out=$4
rootfs=$5
guestd=$6
apko_image=${APKO_IMAGE:-cgr.dev/chainguard/apko@sha256:44ee5c39a8e42006372bd66625ac9be0eef78082777d1fcad57013fa84fe53ed}
tools_image=${ROOTFS_TOOLS_IMAGE:-alpine:3.22.2}
arch=${ARCH:-aarch64}
apko_arch=${APKO_ARCH:-$arch}
apko_lock=${APKO_LOCK:-apko.$apko_arch.lock.json}

case "$arch" in
	aarch64|x86_64) ;;
	*)
		echo "unsupported ARCH: $arch" >&2
		exit 1
		;;
esac

archive="$out/apko.tar"
bundle="$out/bundle"

rm -rf "$archive" "$bundle"
mkdir -p "$out"

if [ "$role" != "guest" ]; then
	echo "unsupported image role: $role" >&2
	exit 1
fi

docker run --rm -v "$repo_root":/work -w "/work/$role_dir" "$apko_image" build apko.yaml "helmr-$role:local" "$archive" --arch "$apko_arch" --lockfile "$apko_lock" --sbom=false

docker run --rm -v "$repo_root":/work -w "/work/$role_dir" \
	-e ROLE="$role" \
	-e ARCH="$arch" \
	-e ARCHIVE="$archive" \
	-e BUNDLE="$bundle" \
	-e OUT="$out" \
	-e ROOTFS="$rootfs" \
	-e GUESTD="$guestd" \
	"$tools_image" sh -ceu '
	trap '"'"'rm -rf "$BUNDLE"'"'"' EXIT

	apk add --no-cache e2fsprogs jq tar
	layers="$BUNDLE/layers"
	root="$BUNDLE/rootfs"
	rm -rf "$BUNDLE"
	mkdir -p "$layers" "$root"
	tar -xf "$ARCHIVE" -C "$layers"
	jq -r ".[0].Layers[]" "$layers/manifest.json" | while IFS= read -r layer; do
		tar \
			--no-same-owner \
			--no-same-permissions \
			--exclude=dev/console \
			--exclude=dev/null \
			--exclude=dev/random \
			--exclude=dev/urandom \
			--exclude=dev/zero \
			-xzf "$layers/$layer" -C "$root"
	done

	install -m 0755 "$GUESTD" "$root/usr/bin/guestd"
	install -m 0755 init.sh "$root/init"
	install -d -m 0755 "$root/sbin"
	ln -sf /init "$root/sbin/init"
	mkdir -p "$root/dev" "$root/tmp" "$root/run" "$root/var/lib/helmr"
	rm -f "$root/etc/resolv.conf"
	printf "nameserver 1.1.1.1\n" > "$root/etc/resolv.conf"
	chmod 1777 "$root/tmp"
	if [ -d "$OUT/initramfs-root/lib/modules" ]; then
		mkdir -p "$root/lib"
		cp -R "$OUT/initramfs-root/lib/modules" "$root/lib/"
	fi

	case "$ROLE" in
		guest)
			mkdir -p "$root/opt/helmr"
			;;
		*)
			echo "unknown role: $ROLE" >&2
			exit 1
			;;
	esac

	rootfs_size_mb=$(du -sm "$root" | awk '"'"'{ size = int($1 * 13 / 10) + 128; if (size < 512) size = 512; print size "M" }'"'"')
	rm -f "$ROOTFS"
	mkfs.ext4 -d "$root" "$ROOTFS" "$rootfs_size_mb"
	e2fsck -fy "$ROOTFS"
	e2fsck -fn "$ROOTFS"
'
