#!/bin/sh
# Inside a DISPOSABLE privileged container only. Creates only owned image files.
set -eu
case "${HELMR_DISPOSABLE_STORAGE_FIXTURE:-}" in 1) ;; *) echo 'Set HELMR_DISPOSABLE_STORAGE_FIXTURE=1 only inside a disposable container' >&2; exit 2;; esac
command -v mkfs.xfs >/dev/null
command -v mkfs.btrfs >/dev/null
fixture=$(mktemp -d /tmp/computer-fs-compare.XXXXXX)
cleanup() {
  mountpoint -q "$fixture/receiver" && umount "$fixture/receiver" || true
  mountpoint -q "$fixture/source" && umount "$fixture/source" || true
}
trap cleanup EXIT
mkdir "$fixture/source" "$fixture/receiver"
uname -r
dpkg-query -W xfsprogs btrfs-progs
for kind in ${HELMR_FIXTURE_ORDER:-xfs btrfs btrfs-nocow}; do
  truncate -s 3G "$fixture/source.img"
  truncate -s 3G "$fixture/receiver.img"
  if [ "$kind" = xfs ]; then
    mkfs.xfs -q -f -m reflink=1 "$fixture/source.img"
    mkfs.xfs -q -f -m reflink=1 "$fixture/receiver.img"
  else
    mkfs.btrfs -q -f "$fixture/source.img"
    mkfs.btrfs -q -f "$fixture/receiver.img"
  fi
  mount -o loop "$fixture/source.img" "$fixture/source"
  mount -o loop "$fixture/receiver.img" "$fixture/receiver"
  python3 /proof/compare.py "$kind" "$fixture/source" "$fixture/receiver" "$fixture/archive-$kind"
  umount "$fixture/receiver"
  umount "$fixture/source"
  rm "$fixture/source.img" "$fixture/receiver.img"
done
