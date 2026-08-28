#!/bin/sh
set -eu

initramfs=$1
base=$2
modloop=$3
kernel_modules=$4
boot_init=$5
tools_image=${BOOT_TOOLS_IMAGE:?BOOT_TOOLS_IMAGE is required}
source_date_epoch=0

for path in "$initramfs" "$base" "$modloop" "$kernel_modules" "$boot_init"; do
	case "$path" in
	/*|../*|*/../*|*/..)
		printf 'boot artifact path must stay within the role directory: %s\n' "$path" >&2
		exit 1
		;;
	esac
done

docker run --rm --platform linux/amd64 \
	-v "$(pwd)":/work \
	-w /work \
	-e BASE="$base" \
	-e HOST_GID="$(id -g)" \
	-e HOST_UID="$(id -u)" \
	-e INITRAMFS="$initramfs" \
	-e KERNEL_MODULES="$kernel_modules" \
	-e MODLOOP="$modloop" \
	-e BOOT_INIT="$boot_init" \
	-e SOURCE_DATE_EPOCH="$source_date_epoch" \
	--entrypoint /bin/sh \
	"$tools_image" -ceu '
	export LC_ALL=C TZ=UTC
	umask 022

	root=$(mktemp -d)
	modroot=$(mktemp -d)
	tmp_initramfs="/work/${INITRAMFS}.tmp"
	tmp_modules="/work/${KERNEL_MODULES}.tmp"
	trap '\''rm -rf "$root" "$modroot" "$tmp_initramfs" "$tmp_modules"'\'' EXIT

	mkdir -p "$root/source" "$root/initramfs" "$root/modules/lib/modules"
	gzip -dc "/work/$BASE" |
		(cd "$root/source" && cpio --quiet --extract --make-directories --unconditional --no-absolute-filenames)
	unsquashfs -f -q -d "$modroot" "/work/$MODLOOP"

	kernel_version=
	count=0
	for candidate in "$modroot/modules"/*; do
		[ -d "$candidate/kernel" ] || continue
		kernel_version=$(basename "$candidate")
		count=$((count + 1))
	done
	if [ "$count" -ne 1 ]; then
		printf "modloop kernel module directory count = %s, want 1\n" "$count" >&2
		exit 1
	fi
	module_src="$modroot/modules/$kernel_version"
	module_dst="$root/modules/lib/modules/$kernel_version"
	if [ ! -d "$module_src/kernel" ]; then
		printf "modloop has no modules for kernel %s\n" "$kernel_version" >&2
		exit 1
	fi

	copy_module() {
		relative=$1
		source="$module_src/$relative"
		destination="$module_dst/$relative"
		if [ ! -f "$source" ]; then
			printf "required kernel module is missing: %s\n" "$relative" >&2
			exit 1
		fi
		install -D -m 0644 "$source" "$destination"
	}

	# Fixed Firecracker topology: root disk, network, squashfs, scratch,
	# Program drives, packet sockets, and guestd vsock transport.
	copy_module kernel/drivers/block/virtio_blk.ko
	copy_module kernel/drivers/net/virtio_net.ko
	copy_module kernel/drivers/net/net_failover.ko
	copy_module kernel/net/core/failover.ko
	copy_module kernel/fs/ext4/ext4.ko
	copy_module kernel/fs/jbd2/jbd2.ko
	copy_module kernel/fs/mbcache.ko
	copy_module kernel/fs/squashfs/squashfs.ko
	copy_module kernel/lib/crc16.ko
	copy_module kernel/net/packet/af_packet.ko
	copy_module kernel/net/vmw_vsock/vsock.ko
	copy_module kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko
	copy_module kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko
	for metadata in "$module_src"/modules.builtin* "$module_src"/modules.order; do
		[ -f "$metadata" ] || continue
		install -m 0644 "$metadata" "$module_dst/$(basename "$metadata")"
	done
	depmod -b "$root/modules" "$kernel_version"

	initroot="$root/initramfs"
	mkdir -p "$initroot/bin" "$initroot/sbin" "$initroot/lib" "$initroot/dev" "$initroot/proc" "$initroot/sys"
	mknod -m 0600 "$initroot/dev/console" c 5 1
	install -m 0755 "$root/source/bin/busybox" "$initroot/bin/busybox"
	install -m 0755 "$root/source/lib/ld-musl-x86_64.so.1" "$initroot/lib/ld-musl-x86_64.so.1"
	ln -s ld-musl-x86_64.so.1 "$initroot/lib/libc.musl-x86_64.so.1"
	for applet in cut mkdir mount sh sleep switch_root; do
		ln -s busybox "$initroot/bin/$applet"
	done
	ln -s ../bin/busybox "$initroot/sbin/modprobe"
	install -m 0755 "/work/$BOOT_INIT" "$initroot/init"
	chroot "$initroot" /sbin/modprobe --help >/dev/null 2>&1

	for relative in \
		kernel/drivers/block/virtio_blk.ko \
		kernel/drivers/net/virtio_net.ko \
		kernel/drivers/net/net_failover.ko \
		kernel/net/core/failover.ko \
		kernel/fs/squashfs/squashfs.ko; do
		install -D -m 0644 "$module_dst/$relative" "$initroot/lib/modules/$kernel_version/$relative"
	done
	for metadata in "$module_dst"/modules.*; do
		[ -f "$metadata" ] || continue
		install -m 0644 "$metadata" "$initroot/lib/modules/$kernel_version/$(basename "$metadata")"
	done

	find "$initroot" "$root/modules" -xdev -exec chown -h 0:0 {} +
	find "$initroot" "$root/modules" -xdev -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

	(
		cd "$initroot"
		find . -xdev -print0 |
			sort -z |
			cpio --null --quiet --create --format=newc --reproducible --owner=0:0 |
			gzip -n -9 >"$tmp_initramfs"
	)
	tar \
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
		-cf "$tmp_modules" \
		-C "$root/modules" lib/modules

	chown "$HOST_UID:$HOST_GID" "$tmp_initramfs" "$tmp_modules"
	mv "$tmp_initramfs" "/work/$INITRAMFS"
	mv "$tmp_modules" "/work/$KERNEL_MODULES"
	trap - EXIT
	rm -rf "$root" "$modroot"
'
