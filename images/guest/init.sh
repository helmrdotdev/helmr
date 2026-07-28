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

disable_user_namespaces() {
	max_namespaces=/proc/sys/user/max_user_namespaces
	if [ ! -w "$max_namespaces" ]; then
		echo "user namespace limit is unavailable" >&2
		exit 1
	fi
	echo 0 > "$max_namespaces"
	if [ "$(cat "$max_namespaces")" != 0 ]; then
		echo "user namespaces remain enabled" >&2
		exit 1
	fi

	unprivileged_clone=/proc/sys/kernel/unprivileged_userns_clone
	if [ -e "$unprivileged_clone" ]; then
		if [ ! -w "$unprivileged_clone" ]; then
			echo "unprivileged user namespace policy is read-only" >&2
			exit 1
		fi
		echo 0 > "$unprivileged_clone"
		if [ "$(cat "$unprivileged_clone")" != 0 ]; then
			echo "unprivileged user namespace cloning remains enabled" >&2
			exit 1
		fi
	fi
}

configure_process_limit() {
	profile=$1
	limit=$(kernel_arg helmr.pids_max || true)
	[ -n "$limit" ] || return 0
	case "$limit" in
		*[!0-9]*|0)
			echo "invalid Helmr process limit" >&2
			exit 1
			;;
	esac
	mkdir -p /sys/fs/cgroup
	is_mounted /sys/fs/cgroup || mount -t cgroup2 cgroup2 /sys/fs/cgroup
	case "$profile" in
		build)
			;;
		*)
		if ! grep -qw pids /sys/fs/cgroup/cgroup.controllers; then
			echo "pids controller unavailable" >&2
			exit 1
		fi
		echo +pids > /sys/fs/cgroup/cgroup.subtree_control
		mkdir /sys/fs/cgroup/helmr
		echo "$limit" > /sys/fs/cgroup/helmr/pids.max
		echo $$ > /sys/fs/cgroup/helmr/cgroup.procs
		if [ "$(cat /sys/fs/cgroup/helmr/pids.max)" != "$limit" ]; then
			echo "Helmr process limit was not applied" >&2
			exit 1
		fi
		return 0
			;;
	esac
	for controller in cpu memory pids; do
		if ! grep -qw "$controller" /sys/fs/cgroup/cgroup.controllers; then
			echo "$controller controller unavailable" >&2
			exit 1
		fi
	done
	echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control
	mkdir /sys/fs/cgroup/helmr
	echo "+cpu +memory +pids" > /sys/fs/cgroup/helmr/cgroup.subtree_control
	mkdir /sys/fs/cgroup/helmr/supervisor
	echo $$ > /sys/fs/cgroup/helmr/supervisor/cgroup.procs
	for controller in cpu memory pids; do
		if ! grep -qw "$controller" /sys/fs/cgroup/helmr/cgroup.subtree_control; then
			echo "$controller controller was not delegated" >&2
			exit 1
		fi
	done
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
			echo "af_packet module load failed; DHCP may not work" >&2
		fi
		if ! modprobe vmw_vsock_virtio_transport; then
			echo "vmw_vsock_virtio_transport module load failed; continuing if initramfs already loaded it" >&2
		fi
	fi
}

install_dhclient_script() {
	cat > /run/helmr-dhclient-script <<'SCRIPT'
#!/bin/sh
set -eu

octet_prefix() {
	case "$1" in
		255) echo 8 ;;
		254) echo 7 ;;
		252) echo 6 ;;
		248) echo 5 ;;
		240) echo 4 ;;
		224) echo 3 ;;
		192) echo 2 ;;
		128) echo 1 ;;
		0) echo 0 ;;
		*)
			echo "unsupported DHCP subnet mask octet: $1" >&2
			exit 1
			;;
	esac
}

mask_to_prefix() {
	old_ifs=$IFS
	IFS=.
	set -- $1
	IFS=$old_ifs
	prefix=0
	for octet in "$@"; do
		prefix=$((prefix + $(octet_prefix "$octet")))
	done
	echo "$prefix"
}

case "${reason:-}" in
	BOUND|RENEW|REBIND|REBOOT)
		prefix=$(mask_to_prefix "$new_subnet_mask")
		ip addr flush dev "$interface"
		ip addr add "$new_ip_address/$prefix" dev "$interface"
		if [ -n "${new_routers:-}" ]; then
			set -- $new_routers
			ip route replace default via "$1" dev "$interface"
		fi
		if [ -n "${new_domain_name_servers:-}" ]; then
			: > /run/resolv.conf
			for nameserver in $new_domain_name_servers; do
				echo "nameserver $nameserver" >> /run/resolv.conf
			done
		fi
		;;
esac
SCRIPT
	chmod 755 /run/helmr-dhclient-script
}

octet_prefix() {
	case "$1" in
		255) echo 8 ;;
		254) echo 7 ;;
		252) echo 6 ;;
		248) echo 5 ;;
		240) echo 4 ;;
		224) echo 3 ;;
		192) echo 2 ;;
		128) echo 1 ;;
		0) echo 0 ;;
		*)
			echo "unsupported static subnet mask octet: $1" >&2
			exit 1
			;;
	esac
}

mask_to_prefix() {
	old_ifs=$IFS
	IFS=.
	set -- $1
	IFS=$old_ifs
	prefix=0
	for octet in "$@"; do
		prefix=$((prefix + $(octet_prefix "$octet")))
	done
	echo "$prefix"
}

kernel_arg() {
	name=$1
	for arg in $(cat /proc/cmdline); do
		case "$arg" in
			"$name"=*) echo "${arg#*=}"; return 0 ;;
		esac
	done
	return 1
}

guest_profile() {
	found=0
	profile=
	for arg in $(cat /proc/cmdline); do
		case "$arg" in
			helmr.profile=*)
				if [ "$found" -ne 0 ]; then
					echo "duplicate Helmr guest profile" >&2
					return 1
				fi
				found=1
				profile=${arg#*=}
				;;
			helmr.profile)
				echo "invalid Helmr guest profile" >&2
				return 1
				;;
		esac
	done
	if [ "$found" -ne 0 ] && [ -z "$profile" ]; then
		echo "invalid empty Helmr guest profile" >&2
		return 1
	fi
	case "$profile" in
		""|build)
			echo "$profile"
			;;
		*)
			echo "invalid Helmr guest profile: $profile" >&2
			return 1
			;;
	esac
}

first_non_loopback_iface() {
	for net in /sys/class/net/*; do
		[ -e "$net" ] || continue
		iface=${net##*/}
		[ "$iface" = "lo" ] && continue
		echo "$iface"
		return 0
	done
	return 1
}

resolve_iface() {
	requested=$1
	if [ -n "$requested" ] && [ -d "/sys/class/net/$requested" ]; then
		echo "$requested"
		return 0
	fi
	first_non_loopback_iface
}

configure_static_from_cmdline() {
	ip_arg=$(kernel_arg ip || true)
	[ -n "$ip_arg" ] || return 1

	client_ip=$(echo "$ip_arg" | cut -d: -f1)
	gateway=$(echo "$ip_arg" | cut -d: -f3)
	netmask=$(echo "$ip_arg" | cut -d: -f4)
	iface=$(echo "$ip_arg" | cut -d: -f6)
	nameserver1=$(echo "$ip_arg" | cut -d: -f8)
	nameserver2=$(echo "$ip_arg" | cut -d: -f9)

	[ -n "$client_ip" ] && [ -n "$gateway" ] && [ -n "$netmask" ] || return 1
	iface=$(resolve_iface "$iface")

	ip link set "$iface" up
	ip addr flush dev "$iface"
	ip addr add "$client_ip/$(mask_to_prefix "$netmask")" dev "$iface"
	ip route replace default via "$gateway" dev "$iface"

	: > /run/resolv.conf
	for nameserver in "$nameserver1" "$nameserver2"; do
		[ -n "$nameserver" ] || continue
		echo "nameserver $nameserver" >> /run/resolv.conf
	done
	[ -s /run/resolv.conf ] || echo "nameserver 1.1.1.1" > /run/resolv.conf
	return 0
}

has_static_network() {
	ip -4 addr show scope global up | grep -q 'inet ' &&
		ip route show default | grep -q '^default '
}

configure_network() {
	if [ "$(kernel_arg helmr.network || true)" = "none" ]; then
		: > /run/resolv.conf
		return
	fi
	echo "nameserver 1.1.1.1" > /run/resolv.conf
	if configure_static_from_cmdline; then
		require_network_ready
		return
	fi
	if has_static_network; then
		require_network_ready
		return
	fi
	install_dhclient_script
	for net in /sys/class/net/*; do
		[ -e "$net" ] || continue
		iface=${net##*/}
		[ "$iface" = "lo" ] && continue
		ip link set "$iface" up
		for attempt in 1 2 3; do
			if dhclient -1 -v -lf /run/dhclient.leases -pf /run/dhclient.pid -sf /run/helmr-dhclient-script "$iface"; then
				require_network_ready
				return
			fi
			echo "dhclient failed on $iface attempt $attempt" >&2
			sleep 1
		done
	done
	echo "no DHCP lease acquired for guest network" >&2
	exit 1
}

require_network_ready() {
	if ! ip route show default | grep -q '^default '; then
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
profile=$(guest_profile)
configure_process_limit "$profile"
if [ -z "$profile" ]; then
	enable_user_namespaces
else
	disable_user_namespaces
fi
mount_scratch
load_vsock
if [ -z "$profile" ]; then
	mount_substrate
	mount_program
	configure_network
else
	configure_network
fi
configure_runtime_identity

export HELMR_GUESTD_TMPDIR=/var/lib/helmr/tmp
if [ -f /etc/helmr/checkpoint-storage-telemetry ]; then
	export HELMR_CHECKPOINT_STORAGE_TELEMETRY=1
fi
set -- /usr/bin/guestd \
	--vsock-port 5000 \
	--health-port 5001
if [ -n "$profile" ]; then
	set -- "$@" --profile "$profile"
fi
exec "$@"
