#!/bin/sh
set -eu

export PATH=/bin:/sbin

fail() {
	printf 'helmr-boot failed: %s\n' "$1" >/dev/console
	exit 1
}

mount -t proc proc /proc || fail 'mount proc'
mount -t sysfs sysfs /sys || fail 'mount sysfs'
mount -t devtmpfs devtmpfs /dev || fail 'mount devtmpfs'

printf 'helmr-boot initramfs-entry uptime=%s\n' "$(cut -d ' ' -f 1 /proc/uptime)" >/dev/console

modprobe virtio_blk || fail 'load virtio_blk'
modprobe virtio_net || fail 'load virtio_net'
modprobe squashfs || fail 'load squashfs'

attempt=0
while [ ! -b /dev/vda ] && [ "$attempt" -lt 40 ]; do
	sleep 0.05
	attempt=$((attempt + 1))
done
[ -b /dev/vda ] || fail 'wait for /dev/vda'

mkdir -p /newroot
mount -t squashfs -o ro /dev/vda /newroot || fail 'mount /dev/vda'
printf 'helmr-boot switch-root uptime=%s\n' "$(cut -d ' ' -f 1 /proc/uptime)" >/dev/console

mkdir -p /newroot/proc /newroot/sys /newroot/dev
mount --move /proc /newroot/proc || fail 'move proc'
mount --move /sys /newroot/sys || fail 'move sysfs'
mount --move /dev /newroot/dev || fail 'move devtmpfs'

exec switch_root /newroot /init
