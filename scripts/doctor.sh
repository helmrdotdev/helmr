#!/bin/sh
set -u

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:-auto}
failures=0
warnings=0

usage() {
	cat <<'EOF'
Usage: scripts/doctor.sh [auto|common|buildkit|linux|all]

Checks whether the current host has the tools and OS facilities needed by
Helmr development and Linux Firecracker smoke tests.
EOF
}

ok() {
	printf 'ok: %s\n' "$1"
}

warn() {
	warnings=$((warnings + 1))
	printf 'warn: %s\n' "$1" >&2
}

fail() {
	failures=$((failures + 1))
	printf 'fail: %s\n' "$1" >&2
}

need_command() {
	if command -v "$1" >/dev/null 2>&1; then
		ok "$2"
	else
		fail "$2 (missing command: $1)"
	fi
}

want_command() {
	if command -v "$1" >/dev/null 2>&1; then
		ok "$2"
	else
		warn "$2 (missing command: $1)"
	fi
}

need_file() {
	if [ -e "$1" ]; then
		ok "$2"
	else
		fail "$2 (missing: $1)"
	fi
}

version_line() {
	if command -v "$1" >/dev/null 2>&1; then
		case "$1" in
			go) version="$($1 version 2>/dev/null | head -n 1)" ;;
			*) version="$($1 --version 2>/dev/null | head -n 1)" ;;
		esac
		printf 'info: %s: %s\n' "$1" "$version"
	fi
}

check_common() {
	printf '== common ==\n'
	need_command go "Go toolchain is available"
	need_command bun "Bun is available for TypeScript protobuf codegen"
	need_command buf "Buf CLI is available"
	need_command git "Git is available"
	want_command docker "Docker client is available for boot artifact image builds"
	want_command jq "jq is available"
	want_command nix "Nix CLI is available in PATH"
	want_command direnv "direnv is available for automatic dev shell activation"
	need_file "$repo_root/go.mod" "Go module file is present"
	need_file "$repo_root/go.sum" "Go checksum file is present"
	need_file "$repo_root/bun.lock" "Bun lockfile is present"

	version_line go
	version_line bun
	version_line buf
	version_line git
	version_line jq
	version_line nix
	version_line direnv
}

check_buildkit() {
	printf '== buildkit ==\n'
	if [ "$(uname -s)" != "Linux" ]; then
		fail "BuildKit worker smoke requires a Linux host"
		return
	fi

	need_command buildkitd "BuildKit daemon binary is available"
	need_command buildctl "BuildKit client is available"
	need_command runc "OCI runtime for BuildKit is available"
	want_command rootlesskit "RootlessKit is available for isolated BuildKit"
	want_command slirp4netns "slirp4netns is available for rootless BuildKit networking"
	want_command fuse-overlayfs "fuse-overlayfs is available for rootless BuildKit snapshots"

	buildkit_addr=${HELMR_WORKER_BUILDKIT_ADDR:-unix:///run/helmr/buildkit/buildkitd.sock}
	case "$buildkit_addr" in
		unix://*)
			buildkit_sock=${buildkit_addr#unix://}
			if [ -S "$buildkit_sock" ]; then
				ok "BuildKit socket exists: $buildkit_sock"
			else
				fail "BuildKit socket is missing: $buildkit_sock"
			fi
			;;
		*)
			warn "BuildKit address is not a unix socket: $buildkit_addr"
			;;
	esac
	if command -v buildctl >/dev/null 2>&1; then
		if buildctl --addr "$buildkit_addr" debug workers >/dev/null 2>&1; then
			ok "BuildKit daemon is reachable"
		else
			fail "BuildKit daemon is not reachable at $buildkit_addr"
		fi
	fi
	if [ -n "${HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE:-}" ]; then
		ok "BuildKit cache namespace is configured: $HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE"
	else
		warn "HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE is unset; worker will use helmr"
	fi
}

check_linux() {
	printf '== linux/firecracker ==\n'
	if [ "$(uname -s)" != "Linux" ]; then
		fail "Linux Firecracker smoke requires a Linux host"
		return
	fi

	need_command firecracker "Firecracker binary is available"
	need_command ip "iproute2 is available"
	need_command iptables "iptables is available"
	want_command nft "nftables is available"

	if [ -c /dev/kvm ]; then
		ok "/dev/kvm exists"
		if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
			ok "/dev/kvm is readable and writable by this user"
		else
			fail "/dev/kvm is not readable and writable by this user"
		fi
	else
		fail "/dev/kvm is missing; KVM or nested virtualization is not available"
	fi

	if [ -n "${HELMR_WORKER_FIRECRACKER_PATH:-}" ]; then
		if [ -x "$HELMR_WORKER_FIRECRACKER_PATH" ]; then
			ok "HELMR_WORKER_FIRECRACKER_PATH points to an executable"
		else
			fail "HELMR_WORKER_FIRECRACKER_PATH is set but not executable: $HELMR_WORKER_FIRECRACKER_PATH"
		fi
	else
		warn "HELMR_WORKER_FIRECRACKER_PATH is unset; the worker will resolve firecracker from PATH"
	fi

	jailer_path=${HELMR_WORKER_FIRECRACKER_JAILER_PATH:-jailer}
	if command -v "$jailer_path" >/dev/null 2>&1 || [ -x "$jailer_path" ]; then
		ok "Firecracker jailer is available: $jailer_path"
	else
		fail "Firecracker jailer is missing or not executable: $jailer_path"
	fi
	if [ -n "${HELMR_WORKER_FIRECRACKER_JAILER_UID:-}" ] && [ "$HELMR_WORKER_FIRECRACKER_JAILER_UID" -gt 0 ] 2>/dev/null; then
		ok "Firecracker jailer uid is configured"
	else
		fail "HELMR_WORKER_FIRECRACKER_JAILER_UID must be a positive integer"
	fi
	if [ -n "${HELMR_WORKER_FIRECRACKER_JAILER_GID:-}" ] && [ "$HELMR_WORKER_FIRECRACKER_JAILER_GID" -gt 0 ] 2>/dev/null; then
		ok "Firecracker jailer gid is configured"
	else
		fail "HELMR_WORKER_FIRECRACKER_JAILER_GID must be a positive integer"
	fi
	ok "Firecracker built-in seccomp filter will be used"
	if [ -n "${HELMR_WORKER_CNI_PROFILE:-}" ]; then
		ok "CNI profile is configured: $HELMR_WORKER_CNI_PROFILE"
	else
		warn "HELMR_WORKER_CNI_PROFILE is unset; checkpoint restore compatibility will default to <network>/v0"
	fi
	if [ -d /sys/fs/cgroup ]; then
		ok "cgroup filesystem is mounted"
	else
		fail "cgroup filesystem is missing"
	fi
	if [ -c /dev/net/tun ]; then
		ok "/dev/net/tun exists"
	else
		fail "/dev/net/tun is missing; CNI tap setup requires tun support"
	fi

	cni_conf_dir=${HELMR_WORKER_CNI_CONF_DIR:-/etc/cni/conf.d}
	cni_bin_dir=${HELMR_WORKER_CNI_BIN_DIR:-/opt/cni/bin}
	cni_network=${HELMR_WORKER_CNI_NETWORK:-helmr}
	if [ -d "$cni_conf_dir" ]; then
		ok "CNI config directory exists: $cni_conf_dir"
	else
		fail "CNI config directory is missing: $cni_conf_dir"
	fi
	if [ -d "$cni_bin_dir" ]; then
		ok "CNI plugin directory exists: $cni_bin_dir"
	else
		fail "CNI plugin directory is missing: $cni_bin_dir"
	fi
	if find "$cni_conf_dir" -maxdepth 1 \( -name '*.conf' -o -name '*.conflist' \) -type f -exec grep -l "\"name\"[[:space:]]*:[[:space:]]*\"$cni_network\"" {} + >/dev/null 2>&1; then
		ok "CNI network is configured: $cni_network"
	else
		fail "CNI network is not configured: $cni_network"
	fi
	for plugin in ptp host-local firewall tc-redirect-tap; do
		if [ -x "$cni_bin_dir/$plugin" ]; then
			ok "CNI plugin is executable: $plugin"
		else
			fail "CNI plugin is missing or not executable: $cni_bin_dir/$plugin"
		fi
	done

	if [ -n "${XDG_DATA_HOME:-}" ]; then
		ok "XDG_DATA_HOME is set"
	else
		warn "XDG_DATA_HOME is unset; smoke-linux will default it under .helmr-smoke"
	fi

	if [ -n "${XDG_RUNTIME_DIR:-}" ]; then
		ok "XDG_RUNTIME_DIR is set"
	else
		warn "XDG_RUNTIME_DIR is unset; smoke-linux will default it under .helmr-smoke"
	fi

	check_buildkit

	ip_forward=$(sysctl -n net.ipv4.ip_forward 2>/dev/null || printf 'unknown')
	if [ "$ip_forward" = "1" ]; then
		ok "IPv4 forwarding is enabled"
	else
		warn "IPv4 forwarding is not enabled (net.ipv4.ip_forward=$ip_forward)"
	fi
}

case "$mode" in
	-h|--help)
		usage
		exit 0
		;;
	auto)
		check_common
		case "$(uname -s)" in
			Linux) check_linux ;;
			Darwin) warn "remote execution workers require Linux Firecracker; skipping VM checks on macOS" ;;
			*) warn "unsupported host OS: $(uname -s)" ;;
		esac
		;;
	common)
		check_common
		;;
	buildkit)
		check_buildkit
		;;
	linux)
		check_common
		check_linux
		;;
	all)
		check_common
		check_linux
		;;
	*)
		usage >&2
		exit 2
		;;
esac

printf 'summary: %s failure(s), %s warning(s)\n' "$failures" "$warnings"
if [ "$failures" -gt 0 ]; then
	exit 1
fi
