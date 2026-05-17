#!/bin/sh
set -eu

initramfs=$1
base=$2
modloop=$3
initramfs_root=$4
modloop_root=$5
tools_image=${ROOTFS_TOOLS_IMAGE:-alpine:3.22.2}

rm -rf "$initramfs_root" "$modloop_root"
mkdir -p "$initramfs_root"

(cd "$initramfs_root" && gzip -dc "../$(basename "$base")" | cpio -idmu)

docker run --rm -v "$(pwd)":/work -w /work \
	-e MODLOOP_ROOT="$modloop_root" \
	-e MODLOOP="$modloop" \
	-e INITRAMFS_ROOT="$initramfs_root" \
	"$tools_image" sh -ceu '
	apk add --no-cache kmod squashfs-tools
	unsquashfs -f -q -d "$MODLOOP_ROOT" "$MODLOOP"
	kernel_version="$(basename "$(find "$INITRAMFS_ROOT/lib/modules" -mindepth 1 -maxdepth 1 -type d | head -n 1)")"
	module_src="$MODLOOP_ROOT/modules/$kernel_version"
	module_dst="$INITRAMFS_ROOT/lib/modules/$kernel_version"
	mkdir -p "$module_dst/kernel/fs/ext4" "$module_dst/kernel/fs/jbd2" "$module_dst/kernel/lib" "$module_dst/kernel/net/packet" "$module_dst/kernel/net/vmw_vsock"
	cp "$module_src/kernel/fs/ext4/ext4.ko" "$module_dst/kernel/fs/ext4/"
	cp "$module_src/kernel/fs/jbd2/jbd2.ko" "$module_dst/kernel/fs/jbd2/"
	cp "$module_src/kernel/fs/mbcache.ko" "$module_dst/kernel/fs/"
	cp "$module_src/kernel/lib/crc16.ko" "$module_dst/kernel/lib/"
	cp "$module_src/kernel/net/packet/af_packet.ko" "$module_dst/kernel/net/packet/"
	cp "$module_src/kernel/net/vmw_vsock/vsock.ko" "$module_dst/kernel/net/vmw_vsock/"
	cp "$module_src/kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko" "$module_dst/kernel/net/vmw_vsock/"
	cp "$module_src/kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko" "$module_dst/kernel/net/vmw_vsock/"
	depmod -b "$INITRAMFS_ROOT" "$kernel_version"
'

(cd "$initramfs_root" && find . | cpio -o -H newc | gzip -9 > "../$(basename "$initramfs")")
