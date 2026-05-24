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

	apk add --no-cache binutils e2fsprogs jq tar
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
	mkdir -p "$root/dev" "$root/tmp" "$root/run"
	rm -f "$root/etc/resolv.conf"
	printf "nameserver 1.1.1.1\n" > "$root/etc/resolv.conf"
	chmod 1777 "$root/tmp"
	if [ -d "$OUT/initramfs-root/lib/modules" ]; then
		mkdir -p "$root/lib"
		cp -R "$OUT/initramfs-root/lib/modules" "$root/lib/"
	fi

	find_runtime_lib() {
		needed=$1
		for dir in "$root/lib" "$root/usr/lib"; do
			if [ -e "$dir/$needed" ]; then
				printf "%s\n" "$dir/$needed"
				return 0
			fi
		done
		found=$(find "$root/lib" "$root/usr/lib" \( -type f -o -type l \) -name "$needed" 2>/dev/null | head -n 1)
		if [ -n "$found" ]; then
			printf "%s\n" "$found"
			return 0
		fi
		return 1
	}

	copy_runtime_lib() {
		runtime_lib_source=$1
		runtime_lib_dest=$2
		while [ -L "$runtime_lib_source" ]; do
			cp -P "$runtime_lib_source" "$runtime_lib_dest/"
			target=$(readlink "$runtime_lib_source")
			case "$target" in
				/*) runtime_lib_source="$root$target" ;;
				*) runtime_lib_source="$(dirname "$runtime_lib_source")/$target" ;;
			esac
		done
		cp -P "$runtime_lib_source" "$runtime_lib_dest/"
	}

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
		  "runtime_contract_version": 1,
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

	copy_elf_runtime_deps() {
		queue="$BUNDLE/runtime-elf-queue"
		queue_next="$BUNDLE/runtime-elf-queue-next"
		dest=$2
		seen=$3
		[ -e "$1" ] || return 0
		printf "%s\n" "$1" > "$queue"
		while [ -s "$queue" ]; do
			elf_path=$(sed -n "1p" "$queue")
			sed "1d" "$queue" > "$queue_next"
			mv "$queue_next" "$queue"
			if grep -Fxq "$elf_path" "$seen" 2>/dev/null; then
				continue
			fi
			printf "%s\n" "$elf_path" >> "$seen"
			if ! readelf -h "$elf_path" >/dev/null 2>&1; then
				continue
			fi

			interp=$(readelf -l "$elf_path" 2>/dev/null | sed -n "s/.*Requesting program interpreter: \\(.*\\)]/\\1/p" | head -n 1)
			if [ -n "$interp" ] && [ -e "$root$interp" ]; then
				copy_runtime_lib "$root$interp" "$dest"
				printf "%s\n" "$root$interp" >> "$queue"
			fi

			readelf -d "$elf_path" 2>/dev/null | sed -n "s/.*Shared library: \\[\\(.*\\)\\]/\\1/p" | while IFS= read -r needed; do
				dep=$(find_runtime_lib "$needed") || {
					echo "missing runtime library $needed required by $elf_path" >&2
					exit 1
				}
				copy_runtime_lib "$dep" "$dest"
				printf "%s\n" "$dep" >> "$queue"
			done
		done
		rm -f "$queue" "$queue_next"
	}

	package_guest_runtime_bundle() {
		runtime="$root/opt/helmr-runtime"
		rm -rf "$runtime"
		mkdir -p "$runtime/bin" "$runtime/lib" "$runtime/adapter"
		install -m 0755 "$root/usr/bin/node" "$runtime/bin/node"
		install -m 0755 "$GUESTD" "$runtime/bin/run-child"
		cp "$root/opt/helmr/adapter/main.js" "$runtime/adapter/main.js"
		cp "$root/opt/helmr/adapter/register.mjs" "$runtime/adapter/register.mjs"
		cp "$root/opt/helmr/adapter/loader.mjs" "$runtime/adapter/loader.mjs"
		cp "$root/opt/helmr/adapter/manifest.json" "$runtime/adapter/manifest.json"

		seen="$BUNDLE/runtime-elf-seen"
		: > "$seen"
		copy_elf_runtime_deps "$runtime/bin/node" "$runtime/lib" "$seen"
		copy_elf_runtime_deps "$runtime/bin/run-child" "$runtime/lib" "$seen"
		rm -f "$seen"
	}

	case "$ROLE" in
		guest)
			mkdir -p "$root/opt/helmr"
			install_adapter_bundle "$root/opt/helmr"
			package_guest_runtime_bundle
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
