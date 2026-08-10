#!/bin/sh
set -eu

role=$1
repo_root=$2
role_dir=$3
out=$4
rootfs=$5
guestd=$6
kernel_modules=$7
tools_image=${BOOT_TOOLS_IMAGE:?BOOT_TOOLS_IMAGE is required}
encoder=${HELMR_SQUASHFS_ENCODER:?HELMR_SQUASHFS_ENCODER is required}
arch=${ARCH:-x86_64}
apko_arch=${APKO_ARCH:-$arch}
apko_lock=${APKO_LOCK:-apko.$apko_arch.lock.json}
source_date_epoch=0

if [ "$arch" != "x86_64" ]; then
	printf 'unsupported ARCH: %s\n' "$arch" >&2
	exit 1
fi
if [ "$role" != "guest" ]; then
	printf 'unsupported image role: %s\n' "$role" >&2
	exit 1
fi

archive="$out/apko.tar"
tmp_archive="${archive}.tmp"
trap 'rm -f "$tmp_archive"' EXIT
mkdir -p "$out"
(
	cd "$repo_root/$role_dir"
	apko build \
		apko.yaml \
		"helmr-$role:local" \
		"$tmp_archive" \
		--arch "$apko_arch" \
		--lockfile "$apko_lock" \
		--build-date 1970-01-01T00:00:00Z \
		--sbom=false \
		--vcs=false
)
mv "$tmp_archive" "$archive"
trap - EXIT

docker run --rm --platform linux/amd64 \
	-v "$repo_root":/work \
	-w "/work/$role_dir" \
	-e ARCHIVE="$archive" \
	-e GUESTD="$guestd" \
	-e HOST_GID="$(id -g)" \
	-e HOST_UID="$(id -u)" \
	-e KERNEL_MODULES="$kernel_modules" \
	-e ROOTFS="$rootfs" \
	-e ROOTFS_TAR="$out/rootfs.tar" \
	-e SOURCE_DATE_EPOCH="$source_date_epoch" \
	--entrypoint /bin/sh \
	"$tools_image" -ceu '
	export LC_ALL=C TZ=UTC
	umask 022

	bundle=$(mktemp -d)
	layers="$bundle/layers"
	root="$bundle/root"
	tmp_tar="$(pwd)/${ROOTFS_TAR}.tmp"
	trap '\''rm -rf "$bundle" "$tmp_tar"'\'' EXIT
	mkdir -p "$layers" "$root"
	tar -xf "$ARCHIVE" -C "$layers"

	layer=$(
		jq -er '\''
			if length == 1 and (.[0].Layers | length) == 1
			then .[0].Layers[0]
			else error("expected one apko image layer")
			end
		'\'' "$layers/manifest.json"
	)
	if tar -tzf "$layers/$layer" | grep -E "(^|/)\\.wh\\." >/dev/null; then
		printf "apko root layer contains unsupported whiteouts\n" >&2
		exit 1
	fi
	tar \
		--numeric-owner \
		--same-owner \
		--same-permissions \
		--delay-directory-restore \
		--exclude=dev/console \
		--exclude=dev/null \
		--exclude=dev/random \
		--exclude=dev/urandom \
		--exclude=dev/zero \
		-xzf "$layers/$layer" \
		-C "$root"

	install -m 0755 "$GUESTD" "$root/usr/bin/guestd"
	install -m 0755 init.sh "$root/init"
	install -d -m 0755 "$root/sbin" "$root/dev" "$root/run" "$root/var/lib/helmr" "$root/opt/helmr"
	install -d -m 1777 "$root/tmp"
	ln -sf /init "$root/sbin/init"
	rm -f "$root/etc/resolv.conf"
	printf "nameserver 1.1.1.1\n" >"$root/etc/resolv.conf"
	tar --numeric-owner --same-owner --same-permissions -xf "$KERNEL_MODULES" -C "$root"

	if find "$root" -xdev \( -type b -o -type c -o -type p -o -type s \) -print -quit | grep -q .; then
		printf "guest root contains a non-regular special file\n" >&2
		exit 1
	fi
	find "$root" -xdev -exec chown -h 0:0 {} +
	find "$root" -xdev -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

	rm -f "$tmp_tar"
	(
		cd "$root"
		find . -mindepth 1 -xdev -printf "%P\\0" |
			sort -z |
			tar \
				--null \
				--no-recursion \
				--files-from=- \
				--sort=name \
				--format=posix \
				--numeric-owner \
				--owner=0 \
				--group=0 \
				--mtime="@$SOURCE_DATE_EPOCH" \
				--clamp-mtime \
				--pax-option=delete=atime,delete=ctime \
				--no-acls \
				--no-xattrs \
				-cf "$tmp_tar"
	)

	chown "$HOST_UID:$HOST_GID" "$tmp_tar"
	mv "$tmp_tar" "$ROOTFS_TAR"
	trap - EXIT
	rm -rf "$bundle"
'

case "$encoder" in
	/*) ;;
	*)
		printf 'SquashFS encoder must be an absolute path: %s\n' "$encoder" >&2
	exit 1
	;;
esac
if [ ! -x "$encoder" ]; then
	printf 'SquashFS encoder is not executable: %s\n' "$encoder" >&2
	exit 1
fi
"$encoder" -version 2>&1 | grep -E '^mksquashfs version 4[.]6[.]1([[:space:]]|$)' >/dev/null

normalized_tar="$out/rootfs.tar"
tmp_rootfs="${rootfs}.tmp"
trap 'rm -f "$normalized_tar" "$tmp_rootfs"' EXIT
rm -f "$tmp_rootfs"
unset SOURCE_DATE_EPOCH
LC_ALL=C TZ=UTC "$encoder" - "$tmp_rootfs" \
	-tar \
	-noappend \
	-all-root \
	-no-xattrs \
	-no-exports \
	-no-fragments \
	-no-tailends \
	-no-duplicates \
	-no-hardlinks \
	-no-progress \
	-exit-on-error \
	-processors 2 \
	-mem 1024M \
	-comp zstd \
	-b 131072 \
	-root-mode 0755 \
	-mkfs-time 0 \
	-all-time 0 <"$normalized_tar"
unsquashfs -s "$tmp_rootfs" >/dev/null
mv "$tmp_rootfs" "$rootfs"
rm -f "$normalized_tar"
trap - EXIT
