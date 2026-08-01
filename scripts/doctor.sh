#!/bin/sh
set -u

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:-auto}
failures=0
warnings=0

usage() {
	cat <<'EOF'
Usage: scripts/doctor.sh [auto|common|linux|all]

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
	if [ -d /sys/fs/cgroup ]; then
		ok "cgroup filesystem is mounted"
	else
		fail "cgroup filesystem is missing"
	fi
	if [ -c /dev/net/tun ]; then
		ok "/dev/net/tun exists"
	else
		fail "/dev/net/tun is missing; routed TAP setup requires tun support"
	fi
	for variable in HELMR_WORKER_NETWORK_LINK_POOL HELMR_WORKER_NETWORK_TRANSLATION_POOL HELMR_WORKER_NETWORK_RESOLVER_IPV4; do
		eval "value=\${$variable:-}"
		if [ -n "$value" ]; then
			ok "$variable is configured"
		else
			fail "$variable is required for the routed network ABI"
		fi
	done
	for command in "${HELMR_WORKER_IP_PATH:-ip}" "${HELMR_WORKER_NFT_PATH:-nft}"; do
		if command -v "$command" >/dev/null 2>&1 || [ -x "$command" ]; then
			ok "routed network command is available: $command"
		else
			fail "routed network command is missing: $command"
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
