#!/usr/bin/env bash
set -euo pipefail

archive=${1:?usage: guest_initramfs_test.sh <initramfs>}
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
listing="$tmp/listing"
payload="$tmp/payload"

gzip -dc "$archive" | cpio --quiet -itv >"$listing"
awk '$1 ~ /^d/ { next } $1 ~ /^l/ { print $(NF - 2); next } { print $NF }' "$listing" >"$payload"

for required in init bin/busybox sbin/modprobe lib/ld-musl-x86_64.so.1; do
	grep -Fx "$required" "$payload" >/dev/null || {
		printf 'guest initramfs is missing %s\n' "$required" >&2
		exit 1
	}
done
gzip -dc "$archive" | (cd "$tmp" && cpio --quiet -i init)
cmp "$tmp/init" "$repo_root/images/guest/initramfs-init.sh"

unexpected="$tmp/unexpected"
while read -r path; do
	case "$path" in
		init|bin/busybox|bin/cut|bin/mkdir|bin/mount|bin/sh|bin/sleep|bin/switch_root|dev/console|sbin/modprobe|lib/ld-musl-x86_64.so.1|lib/libc.musl-x86_64.so.1) : ;;
		lib/modules/*/modules.*) : ;;
		lib/modules/*/kernel/drivers/block/virtio_blk.ko) : ;;
		lib/modules/*/kernel/drivers/net/virtio_net.ko) : ;;
		lib/modules/*/kernel/drivers/net/net_failover.ko) : ;;
		lib/modules/*/kernel/net/core/failover.ko) : ;;
		lib/modules/*/kernel/fs/squashfs/squashfs.ko) : ;;
		*) printf '%s\n' "$path" ;;
	esac
	done <"$payload" >"$unexpected"
if [ -s "$unexpected" ]; then
	printf 'unexpected guest initramfs content:\n' >&2
	cat "$unexpected" >&2
	exit 1
fi

special=$(awk '$1 ~ /^[cbps]/' "$listing")
if [ "$(printf '%s\n' "$special" | grep -c .)" -ne 1 ] ||
	! printf '%s\n' "$special" | grep -Eq '^crw------- .* 5, *1 .* (\./)?dev/console$'; then
	printf 'guest initramfs special nodes must contain only dev/console 5:1 mode 0600:\n%s\n' "$special" >&2
	exit 1
fi

printf 'ok - guest initramfs content\n'
