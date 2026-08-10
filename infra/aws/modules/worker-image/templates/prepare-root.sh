#!/bin/sh
set -eu

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

[ "$#" -eq 1 ] || fail "usage: helmr-prepare-root EXPECTED_DEVICE_BYTES"
expected_device_bytes=$1
case "$expected_device_bytes" in
  ''|*[!0-9]*) fail "expected root device bytes must be a positive integer" ;;
esac
[ "$expected_device_bytes" -gt 0 ] || fail "expected root device bytes must be positive"

for name in blockdev cat findmnt growpart lsblk readlink resize2fs; do
  command -v "$name" >/dev/null 2>&1 || fail "required root preparation command is unavailable: $name"
done

root_source="$(findmnt -n -o SOURCE --target /)"
root_filesystem="$(findmnt -n -o FSTYPE --target /)"
[ "$root_filesystem" = ext4 ] || fail "Worker root filesystem must be ext4; observed $root_filesystem"
root_partition="$(readlink -f "$root_source")"

# The raw lsblk record is intentionally split into its four whitespace-free fields.
# shellcheck disable=SC2046
set -- $(lsblk --nodeps --noheadings --raw --output TYPE,KNAME,PKNAME,PARTN -- "$root_partition")
[ "$#" -eq 4 ] || fail "Worker root source must resolve to one partition"
root_type=$1
root_kname=$2
parent_kname=$3
partition_number=$4
[ "$root_type" = part ] || fail "Worker root source must be a partition; observed $root_type"
for value in "$root_kname" "$parent_kname"; do
  case "$value" in
    ''|*[!A-Za-z0-9._-]*) fail "Worker root block-device identity is invalid" ;;
  esac
done
case "$partition_number" in
  ''|*[!0-9]*) fail "Worker root partition number is invalid" ;;
esac
[ "$partition_number" -gt 0 ] || fail "Worker root partition number must be positive"

parent_device="/dev/$parent_kname"
parent_type="$(lsblk --nodeps --noheadings --raw --output TYPE -- "$parent_device")"
[ "$parent_type" = disk ] || fail "Worker root parent must be a disk; observed $parent_type"
actual_device_bytes="$(blockdev --getsize64 "$parent_device")"
[ "$actual_device_bytes" = "$expected_device_bytes" ] ||
  fail "Worker root device does not match the configured Launch Template size"

growpart_status=0
growpart "$parent_device" "$partition_number" || growpart_status=$?

parent_sectors="$(cat "/sys/class/block/$parent_kname/size")"
partition_start="$(cat "/sys/class/block/$root_kname/start")"
partition_sectors="$(cat "/sys/class/block/$root_kname/size")"
for value in "$parent_sectors" "$partition_start" "$partition_sectors"; do
  case "$value" in
    ''|*[!0-9]*) fail "Worker root partition geometry is invalid" ;;
  esac
  [ "$value" -gt 0 ] || fail "Worker root partition geometry must be positive"
done
[ "$((parent_sectors * 512))" -eq "$actual_device_bytes" ] ||
  fail "Worker root device geometry disagrees with its raw size"
partition_end=$((partition_start + partition_sectors))
[ "$partition_end" -le "$parent_sectors" ] || fail "Worker root partition extends beyond its parent device"
remaining_sectors=$((parent_sectors - partition_end))
[ "$remaining_sectors" -le 2048 ] ||
  fail "Worker root partition did not consume the configured device after growpart (status $growpart_status)"

resize2fs "$root_partition"

mounted_partition="$(readlink -f "$(findmnt -n -o SOURCE --target /)")"
mounted_filesystem="$(findmnt -n -o FSTYPE --target /)"
mounted_kname="$(lsblk --nodeps --noheadings --raw --output KNAME -- "$mounted_partition")"
[ "$mounted_filesystem" = ext4 ] &&
  [ "$mounted_partition" = "$root_partition" ] &&
  [ "$mounted_kname" = "$root_kname" ] ||
  fail "Worker root mount changed during filesystem preparation"
