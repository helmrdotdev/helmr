#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp_root=${RUNNER_TEMP:-$(mktemp -d)}
workdir=$(mktemp -d "${tmp_root%/}/helmr-buildkit-smoke.XXXXXX")
log_file="$workdir/buildkitd.log"
socket="$workdir/buildkitd.sock"
state_dir="$workdir/state"
runtime_dir="$workdir/runtime"
home_dir="$workdir/home"
addr="unix://$socket"

cleanup() {
	if [ -n "${buildkit_pid:-}" ]; then
		kill "$buildkit_pid" >/dev/null 2>&1 || true
		wait "$buildkit_pid" >/dev/null 2>&1 || true
	fi
	if [ "${KEEP_HELMR_BUILDKIT_SMOKE:-0}" != "1" ]; then
		rm -rf "$workdir"
	fi
}
trap cleanup EXIT

install_prerequisites() {
	sudo apt-get update
	sudo apt-get install -y --no-install-recommends \
		ca-certificates \
		curl \
		fuse-overlayfs \
		uidmap \
		rootlesskit \
		runc \
		slirp4netns
}

install_buildkit() {
	if command -v buildkitd >/dev/null 2>&1 && command -v buildctl >/dev/null 2>&1; then
		return
	fi

	version=$(cd "$repo_root" && go list -m -f '{{.Version}}' github.com/moby/buildkit)
	case "$(uname -m)" in
		x86_64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) echo "unsupported runner architecture: $(uname -m)" >&2; exit 1 ;;
	esac

	archive="buildkit-${version}.linux-${arch}.tar.gz"
	url="https://github.com/moby/buildkit/releases/download/${version}/${archive}"
	curl -fsSL -o "$workdir/$archive" "$url"
	tar -C "$workdir" -xzf "$workdir/$archive"
	sudo install -m 0755 "$workdir/bin/buildkitd" /usr/local/bin/buildkitd
	sudo install -m 0755 "$workdir/bin/buildctl" /usr/local/bin/buildctl
}

configure_rootless_host() {
	sudo sysctl -w user.max_user_namespaces=16384 >/dev/null || true
	if [ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
		sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 >/dev/null || true
	fi

	user_name=$(id -un)
	if ! grep -q "^${user_name}:" /etc/subuid; then
		echo "${user_name}:100000:65536" | sudo tee -a /etc/subuid >/dev/null
	fi
	if ! grep -q "^${user_name}:" /etc/subgid; then
		echo "${user_name}:100000:65536" | sudo tee -a /etc/subgid >/dev/null
	fi
}

start_buildkit() {
	mkdir -p "$state_dir" "$runtime_dir" "$home_dir"
	chmod 0700 "$runtime_dir" "$home_dir"

	snapshotter=${HELMR_BUILDKIT_SMOKE_SNAPSHOTTER:-fuse-overlayfs}
	if [ "$snapshotter" = "fuse-overlayfs" ] && [ ! -c /dev/fuse ]; then
		snapshotter=native
	fi
	printf 'using BuildKit snapshotter: %s\n' "$snapshotter"

	HOME="$home_dir" XDG_RUNTIME_DIR="$runtime_dir" \
		rootlesskit --net=slirp4netns --copy-up=/etc --disable-host-loopback \
		buildkitd \
		--addr "$addr" \
		--root "$state_dir" \
		--oci-worker=true \
		--oci-worker-snapshotter="$snapshotter" \
		>"$log_file" 2>&1 &
	buildkit_pid=$!

	for _ in $(seq 1 60); do
		if buildctl --addr "$addr" debug workers >/dev/null 2>&1; then
			return
		fi
		if ! kill -0 "$buildkit_pid" >/dev/null 2>&1; then
			cat "$log_file" >&2
			echo "buildkitd exited before becoming ready" >&2
			exit 1
		fi
		sleep 1
	done

	cat "$log_file" >&2
	echo "buildkitd did not become ready" >&2
	exit 1
}

solve_without_docker() {
	context_dir="$workdir/context"
	output_dir="$workdir/output"
	mkdir -p "$context_dir" "$output_dir"
	cat > "$context_dir/Dockerfile" <<'EOF'
FROM busybox:1.36.1
RUN printf 'hello from buildkit\n' > /hello.txt
EOF

	buildctl --addr "$addr" build \
		--frontend dockerfile.v0 \
		--local "context=$context_dir" \
		--local "dockerfile=$context_dir" \
		--output "type=local,dest=$output_dir"

	test "$(cat "$output_dir/hello.txt")" = "hello from buildkit"
}

solve_through_helmr() {
	(
		cd "$repo_root"
		HELMR_BUILDKIT_E2E=1 \
			HELMR_WORKER_BUILDKIT_ADDR="$addr" \
			HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE="gha-smoke" \
			go test ./internal/buildkit -run TestBuildKitE2E -count=1 -v
	)
}

install_prerequisites
install_buildkit
configure_rootless_host
start_buildkit

export HELMR_WORKER_BUILDKIT_ADDR="$addr"
export HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE="gha-smoke"
"$repo_root/scripts/doctor.sh" buildkit
solve_without_docker
solve_through_helmr
