#!/bin/sh
set -eu

checksum_file=$1
base_url=$2
out=$3

checksum_dir=$(CDPATH='' cd -- "$(dirname -- "$checksum_file")" && pwd)
checksum_file="$checksum_dir/$(basename "$checksum_file")"
mkdir -p "$out"

expected=$(printf '%s\n' initramfs-virt modloop-virt vmlinuz-virt)
actual=$(awk '
	NF == 2 && $1 ~ /^[0-9a-f]{64}$/ { print $2; next }
	{ exit 2 }
' "$checksum_file" | LC_ALL=C sort)
if [ "$actual" != "$expected" ]; then
	printf 'netboot checksum file must contain exactly initramfs-virt, modloop-virt, and vmlinuz-virt\n' >&2
	exit 1
fi

for name in initramfs-virt modloop-virt vmlinuz-virt; do
	want=$(awk -v name="$name" '$2 == name { print $1 }' "$checksum_file")
	destination="$out/$name"
	got=
	if [ -f "$destination" ]; then
		got=$(sha256sum "$destination" | awk '{print $1}')
	fi
	if [ "$got" = "$want" ]; then
		continue
	fi
	tmp="${destination}.tmp"
	trap 'rm -f "$tmp"' EXIT
	curl -fL --output "$tmp" "$base_url/$name"
	got=$(sha256sum "$tmp" | awk '{print $1}')
	if [ "$got" != "$want" ]; then
		printf 'netboot digest mismatch for %s: got %s, want %s\n' "$name" "$got" "$want" >&2
		exit 1
	fi
	mv "$tmp" "$destination"
	trap - EXIT
done

(
	cd "$out"
	sha256sum -c "$checksum_file" >/dev/null
)
