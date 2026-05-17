#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
platform=${CI_LINUX_NIX_PLATFORM:-linux/amd64}
image=${CI_LINUX_NIX_IMAGE:-nixos/nix:latest}

usage() {
	cat <<'EOF'
Usage: scripts/ci-linux-nix-docker.sh

Runs the GitHub Actions nix workflow inside a Linux Docker container.
Defaults to linux/amd64 to match ubuntu-latest CI.
EOF
}

case "${1:-}" in
	"") ;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 2
		;;
esac

docker run --rm \
	--platform "$platform" \
	--privileged \
	--security-opt seccomp=unconfined \
	--mount type=bind,source="$repo_root",target=/work \
	-w /work \
	"$image" \
	sh -ceu '
		nix --version
		nix --extra-experimental-features "nix-command flakes" \
			flake check --print-build-logs \
			--option sandbox false \
			--option filter-syscalls false
	'
