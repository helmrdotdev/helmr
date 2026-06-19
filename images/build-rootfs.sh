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
adapter_build="$out/adapter-build"

rm -rf "$archive" "$bundle"
mkdir -p "$out"

if [ "$role" != "guest" ]; then
	echo "unsupported image role: $role" >&2
	exit 1
fi

out_abs=$(cd "$out" && pwd)
rm -rf "$adapter_build"
mkdir -p "$adapter_build"
(
	cd "$repo_root"
	bun build runtime/typescript/src/main.ts --target=node --format=esm --outfile="$out_abs/adapter-build/main.js"
)

source_rev=$(git -C "$repo_root" rev-parse HEAD)
dirty=$(git -C "$repo_root" diff --quiet && echo "false" || echo "true")
node_version=$(cd "$repo_root" && node --version)
guestd_version=$(git -C "$repo_root" rev-parse --short HEAD)

docker run --rm -v "$repo_root":/work -w "/work/$role_dir" "$apko_image" build apko.yaml "helmr-$role:local" "$archive" --arch "$apko_arch" --lockfile "$apko_lock" --sbom=false

docker run --rm -v "$repo_root":/work -w "/work/$role_dir" \
	-e ROLE="$role" \
	-e ARCH="$arch" \
	-e ARCHIVE="$archive" \
	-e ADAPTER_BUILD="$adapter_build" \
	-e BUNDLE="$bundle" \
	-e OUT="$out" \
	-e ROOTFS="$rootfs" \
	-e GUESTD="$guestd" \
	-e SOURCE_REV="$source_rev" \
	-e DIRTY="$dirty" \
	-e NODE_VERSION="$node_version" \
	-e GUESTD_VERSION="$guestd_version" \
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

	install_adapter_bundle() {
		helmr_home=$1
		mkdir -p "$helmr_home/adapter"
		install -m 0644 "$ADAPTER_BUILD/main.js" "$helmr_home/adapter/main.js"
		install -m 0644 ../../runtime/typescript/src/register.mjs "$helmr_home/adapter/register.mjs"
		install -m 0644 ../../runtime/typescript/src/loader.mjs "$helmr_home/adapter/loader.mjs"
		ADAPTER_HASH=$(sha256sum "$helmr_home/adapter/main.js" | awk '"'"'{print $1}'"'"')
		PROTO_HASH=$(sha256sum ../../proto/*.proto | sha256sum | awk '"'"'{print $1}'"'"')
		cat > "$helmr_home/adapter/manifest.json" <<-EOF
		{
		  "runtime_contract_version": 0,
		  "adapter_hash": "sha256:$ADAPTER_HASH",
		  "proto_schema_hash": "sha256:$PROTO_HASH",
		  "node_version": "$NODE_VERSION",
		  "guestd_version": "$GUESTD_VERSION",
		  "source_revision": "$SOURCE_REV",
		  "dirty": $DIRTY
		}
		EOF
		cp "$helmr_home/adapter/manifest.json" "$OUT/manifest.json"
	}

	package_guest_adapter_bundle() {
		adapter_bundle="$root/opt/helmr-adapter"
		rm -rf "$adapter_bundle"
		mkdir -p "$adapter_bundle/adapter"
		cp "$root/opt/helmr/adapter/main.js" "$adapter_bundle/adapter/main.js"
		cp "$root/opt/helmr/adapter/register.mjs" "$adapter_bundle/adapter/register.mjs"
		cp "$root/opt/helmr/adapter/loader.mjs" "$adapter_bundle/adapter/loader.mjs"
		cp "$root/opt/helmr/adapter/manifest.json" "$adapter_bundle/adapter/manifest.json"
	}

	case "$ROLE" in
		guest)
			mkdir -p "$root/opt/helmr"
			install_adapter_bundle "$root/opt/helmr"
			package_guest_adapter_bundle
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
