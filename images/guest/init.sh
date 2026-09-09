#!/bin/sh
set -eu

is_mounted() {
	[ -r /proc/mounts ] && grep -qs " $1 " /proc/mounts
}

ensure_char_device() {
	path=$1
	major=$2
	minor=$3
	mode=$4
	if [ ! -c "$path" ]; then
		rm -f "$path"
		mknod -m "$mode" "$path" c "$major" "$minor"
	fi
}

mount_base() {
	is_mounted /proc || mount -t proc proc /proc
	is_mounted /sys || mount -t sysfs sysfs /sys
	if ! is_mounted /dev; then
		if ! mount -t devtmpfs devtmpfs /dev; then
			echo "devtmpfs unavailable; mounting tmpfs on /dev" >&2
			mount -t tmpfs tmpfs /dev
		fi
	fi

	ensure_char_device /dev/null 1 3 666
	ensure_char_device /dev/tty 5 0 666
	mkdir -p /dev/pts
	is_mounted /dev/pts || mount -t devpts -o mode=0620,ptmxmode=0666 devpts /dev/pts
	if [ ! -e /dev/ptmx ]; then
		ln -s pts/ptmx /dev/ptmx
	fi
	mkdir -p /dev/shm
	is_mounted /dev/shm || mount -t tmpfs -o mode=1777,nosuid,nodev,noexec tmpfs /dev/shm
	is_mounted /tmp || mount -t tmpfs -o mode=1777 tmpfs /tmp
	is_mounted /run || mount -t tmpfs -o mode=0755 tmpfs /run
}

enable_user_namespaces() {
	if [ -w /proc/sys/user/max_user_namespaces ]; then
		echo 16384 > /proc/sys/user/max_user_namespaces
	fi
	if [ -w /proc/sys/kernel/unprivileged_userns_clone ]; then
		echo 1 > /proc/sys/kernel/unprivileged_userns_clone
	fi
}

configure_program_cgroups() {
	mkdir -p /sys/fs/cgroup
	is_mounted /sys/fs/cgroup || mount -t cgroup2 cgroup2 /sys/fs/cgroup
	mkdir /sys/fs/cgroup/helmr
	mkdir /sys/fs/cgroup/helmr/supervisor
	echo $$ > /sys/fs/cgroup/helmr/supervisor/cgroup.procs
	if [ -n "$(cat /sys/fs/cgroup/helmr/cgroup.procs)" ]; then
		echo "Helmr Program cgroup root is not process-free" >&2
		exit 1
	fi
	if ! grep -qx '0::/helmr/supervisor' /proc/self/cgroup; then
		echo "Helmr supervisor cgroup placement failed" >&2
		exit 1
	fi
}

mount_scratch() {
	mkdir -p /var/lib/helmr
	if ! is_mounted /var/lib/helmr; then
		if [ ! -b /dev/vdb ]; then
			echo "missing required Helmr scratch disk /dev/vdb" >&2
			exit 1
		fi
		mount -t ext4 -o rw /dev/vdb /var/lib/helmr
	fi
	mkdir -p /var/lib/helmr/tmp
	chmod 1777 /var/lib/helmr/tmp
}

mount_substrate() {
	if [ "$(kernel_arg helmr.substrate || true)" != 1 ]; then
		return 0
	fi
	if [ ! -b /dev/vdc ]; then
		echo "missing required Helmr runtime substrate /dev/vdc" >&2
		exit 1
	fi
	mkdir -p /var/lib/helmr/substrate
	if ! is_mounted /var/lib/helmr/substrate; then
		mount -t ext4 -o ro /dev/vdc /var/lib/helmr/substrate
	fi
	export HELMR_GUESTD_SUBSTRATE_ROOT=/var/lib/helmr/substrate
}

mount_program() {
	if [ "$(kernel_arg helmr.program || true)" != 1 ]; then
		return 0
	fi
	if [ "$(kernel_arg helmr.substrate || true)" = 1 ]; then
		runtime_device=/dev/vdd
		program_device=/dev/vde
	else
		runtime_device=/dev/vdc
		program_device=/dev/vdd
	fi
	for device in "$runtime_device" "$program_device"; do
		if [ ! -b "$device" ]; then
			echo "missing required Helmr Program drive $device" >&2
			exit 1
		fi
	done
	mkdir -p \
		/var/lib/helmr/program/runtime \
		/var/lib/helmr/program/artifact
	mount -t squashfs -o ro,nodev,nosuid "$runtime_device" /var/lib/helmr/program/runtime
	mount -t squashfs -o ro,nodev,nosuid "$program_device" /var/lib/helmr/program/artifact
}

load_vsock() {
	if command -v modprobe >/dev/null 2>&1 && [ -d /lib/modules ]; then
		if ! modprobe af_packet; then
			echo "af_packet module load failed" >&2
		fi
		if ! modprobe vmw_vsock_virtio_transport; then
			echo "vmw_vsock_virtio_transport module load failed; continuing if initramfs already loaded it" >&2
		fi
	fi
}

valid_ipv4() (
	case "$1" in ''|.*|*.|*..*|*[!0-9.]*) return 1 ;; esac
	IFS=.
	set -- $1
	[ "$#" -eq 4 ] || return 1
	for octet do
		case "$octet" in ''|0[0-9]*) return 1 ;; esac
		[ "${#octet}" -le 3 ] && [ "$octet" -le 255 ] || return 1
	done
)

mask_to_prefix() (
	valid_ipv4 "$1" || return 1
	IFS=.
	set -- $1
	prefix=0
	tail=0
	for octet do
		[ "$tail" -eq 0 ] || [ "$octet" -eq 0 ] || return 1
		case "$octet" in
			255) bits=8 ;; 254) bits=7 ;; 252) bits=6 ;; 248) bits=5 ;;
			240) bits=4 ;; 224) bits=3 ;; 192) bits=2 ;; 128) bits=1 ;;
			0) bits=0 ;; *) return 1 ;;
		esac
		prefix=$((prefix + bits))
		[ "$bits" -eq 8 ] || tail=1
	done
	[ "$prefix" -gt 0 ] || return 1
	echo "$prefix"
)

kernel_arg() {
	name=$1
	for arg in $(cat /proc/cmdline); do
		case "$arg" in
			"$name"=*) echo "${arg#*=}"; return 0 ;;
		esac
	done
	return 1
}

configure_static_network() (
	# Preserve the colon layout, but only root init owns this configuration.
	# A sentinel preserves trailing empty fields during POSIX field splitting.
	case "$1" in ''|*[!0-9a-z.:]*) return 1 ;; esac
	ip_fields=$1:end
	IFS=:
	set -- $ip_fields
	[ "$#" -eq 11 ] || return 1
	[ -z "$2$5${9}${10}" ] && [ "$6" = eth0 ] && [ "$7" = off ] || return 1
	client_ip=$1
	gateway=$3
	netmask=$4
	iface=$6
	nameserver=$8
	valid_ipv4 "$client_ip" && valid_ipv4 "$gateway" && valid_ipv4 "$nameserver" || return 1
	[ "$client_ip" != 0.0.0.0 ] && [ "$gateway" != 0.0.0.0 ] && [ "$nameserver" != 0.0.0.0 ] || return 1
	prefix=$(mask_to_prefix "$netmask") || return 1

	# Explicit returns also apply when our caller is an if/AND condition.
	ip link show dev "$iface" >/dev/null || return 1
	ip link set "$iface" up || return 1
	ip addr flush dev "$iface" || return 1
	ip addr add "$client_ip/$prefix" dev "$iface" || return 1
	ip route replace default via "$gateway" dev "$iface" || return 1
	printf 'nameserver %s\n' "$nameserver" > /run/resolv.conf || return 1
)

configure_network() (
	set -f
	ip_arg=
	network=
	cmdline=$(cat /proc/cmdline) || return 1
	for arg in $cmdline; do
		case "$arg" in
			helmr.ip=*)
				[ -z "$ip_arg" ] || return 1
				ip_arg=${arg#*=}
				[ -n "$ip_arg" ] || return 1
				;;
			helmr.network=*)
				[ -z "$network" ] || return 1
				network=${arg#*=}
				[ "$network" = none ] || return 1
				;;
			helmr.ip|helmr.network|ip|ip=*|nfsaddrs|nfsaddrs=*) return 1 ;;
		esac
	done
	if [ "$network" = none ]; then
		[ -z "$ip_arg" ] || return 1
		: > /run/resolv.conf || return 1
		return 0
	fi
	configure_static_network "$ip_arg" || return 1
	require_network_ready
)

require_network_ready() {
	routes=$(ip route show default) || return 1
	if ! printf '%s\n' "$routes" | grep -q '^default '; then
		echo "guest network is missing a default route" >&2
		exit 1
	fi
	if [ ! -s /run/resolv.conf ]; then
		echo "guest network resolver contract is empty" >&2
		exit 1
	fi
}

configure_runtime_identity() {
	hostname helmr-sandbox || true
}

mount_base
configure_program_cgroups
enable_user_namespaces
mount_scratch
load_vsock
mount_substrate
mount_program
configure_network
configure_runtime_identity

export HELMR_GUESTD_TMPDIR=/var/lib/helmr/tmp
exec /usr/bin/guestd \
	--vsock-port 5000 \
	--health-port 5001
