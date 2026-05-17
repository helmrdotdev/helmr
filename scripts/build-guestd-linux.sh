#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
arch=${ARCH:-aarch64}

case "$arch" in
	aarch64) goarch=arm64 ;;
	x86_64) goarch=amd64 ;;
	*)
		echo "unsupported ARCH: $arch" >&2
		exit 1
		;;
esac

output=${GUESTD_OUTPUT:-"$repo_root/dist/guestd/$arch/guestd"}
mkdir -p "$(dirname "$output")"

cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build \
	-trimpath \
	-ldflags="-s -w" \
	-o "$output" \
	./cmd/guestd
