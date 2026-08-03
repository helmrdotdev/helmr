#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
arch=${ARCH:-x86_64}

if [ "$arch" != "x86_64" ]; then
	echo "unsupported ARCH: $arch" >&2
	exit 1
fi

output=${GUESTD_OUTPUT:-"$repo_root/dist/guestd/$arch/guestd"}
mkdir -p "$(dirname "$output")"

cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
	-trimpath \
	-ldflags="-s -w" \
	-o "$output" \
	./cmd/guestd
