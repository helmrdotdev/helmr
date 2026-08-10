#!/usr/bin/env bash
set -euo pipefail

module_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${module_root}/templates/prepare-root.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
bin="${tmp}/bin"
log="${tmp}/calls.log"
state="${tmp}/partition-state"
mkdir -p "${bin}"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

cat >"${bin}/mock-command" <<'EOF'
#!/bin/sh
set -eu
name=${0##*/}
printf '%s %s\n' "$name" "$*" >>"$MOCK_LOG"
case "$name" in
  findmnt)
    case "$*" in
      *FSTYPE*) printf '%s\n' "${MOCK_FILESYSTEM:-ext4}" ;;
      *) printf '/dev/root\n' ;;
    esac
    ;;
  readlink)
    printf '/dev/nvme0n1p1\n'
    ;;
  lsblk)
    case "$*" in
      *TYPE,KNAME,PKNAME,PARTN*)
        if [ "${MOCK_MISSING_PARENT:-0}" = 1 ]; then
          printf 'part nvme0n1p1\n'
        else
          printf 'part nvme0n1p1 nvme0n1 1\n'
        fi
        ;;
      *'--output TYPE -- /dev/nvme0n1'*) printf 'disk\n' ;;
      *'--output KNAME -- /dev/nvme0n1p1'*) printf 'nvme0n1p1\n' ;;
      *) exit 64 ;;
    esac
    ;;
  blockdev)
    [ "$1" = --getsize64 ] && [ "$2" = /dev/nvme0n1 ] || exit 64
    printf '%s\n' "${MOCK_DEVICE_BYTES:-128849018880}"
    ;;
  growpart)
    [ "$1" = /dev/nvme0n1 ] && [ "$2" = 1 ] || exit 64
    case "${MOCK_GROWPART:-grow}" in
      grow) printf 'grown\n' >"$MOCK_STATE" ;;
      nochange) exit 1 ;;
      stuck) exit 2 ;;
      *) exit 64 ;;
    esac
    ;;
  cat)
    case "$1" in
      /sys/class/block/nvme0n1/size) printf '251658240\n' ;;
      /sys/class/block/nvme0n1p1/start) printf '2048\n' ;;
      /sys/class/block/nvme0n1p1/size)
        IFS= read -r partition_state <"$MOCK_STATE"
        if [ "$partition_state" = grown ]; then
          printf '251656159\n'
        else
          printf '50329567\n'
        fi
        ;;
      *) exit 64 ;;
    esac
    ;;
  resize2fs)
    [ "$1" = /dev/nvme0n1p1 ] || exit 64
    [ "${MOCK_RESIZE_FAIL:-0}" != 1 ]
    ;;
  *) exit 64 ;;
esac
EOF
chmod 0755 "${bin}/mock-command"
for name in blockdev cat findmnt growpart lsblk readlink resize2fs; do
  ln -s mock-command "${bin}/${name}"
done

run_helper() {
  PATH="${bin}" \
    MOCK_LOG="${log}" \
    MOCK_STATE="${state}" \
    MOCK_FILESYSTEM="${MOCK_FILESYSTEM:-ext4}" \
    MOCK_DEVICE_BYTES="${MOCK_DEVICE_BYTES:-128849018880}" \
    MOCK_GROWPART="${MOCK_GROWPART:-grow}" \
    MOCK_MISSING_PARENT="${MOCK_MISSING_PARENT:-0}" \
    MOCK_RESIZE_FAIL="${MOCK_RESIZE_FAIL:-0}" \
    /bin/sh "${script}" 128849018880
}

printf 'small\n' >"${state}"
: >"${log}"
run_helper
grep -Fx 'growpart /dev/nvme0n1 1' "${log}" >/dev/null || fail "growpart invocation"
grep -Fx 'resize2fs /dev/nvme0n1p1' "${log}" >/dev/null || fail "resize2fs invocation"

printf 'grown\n' >"${state}"
MOCK_GROWPART=nochange run_helper
MOCK_GROWPART=nochange run_helper

printf 'small\n' >"${state}"
if MOCK_GROWPART=stuck run_helper >/dev/null 2>&1; then
  fail "unfinished partition geometry must fail"
fi
if MOCK_FILESYSTEM=xfs run_helper >/dev/null 2>&1; then
  fail "unsupported root filesystem must fail"
fi
if MOCK_MISSING_PARENT=1 run_helper >/dev/null 2>&1; then
  fail "missing parent device must fail"
fi
if MOCK_DEVICE_BYTES=25769803776 run_helper >/dev/null 2>&1; then
  fail "unexpected raw device size must fail"
fi
printf 'grown\n' >"${state}"
if MOCK_RESIZE_FAIL=1 MOCK_GROWPART=nochange run_helper >/dev/null 2>&1; then
  fail "resize2fs failure must fail"
fi
rm "${bin}/resize2fs"
if MOCK_GROWPART=nochange run_helper >/dev/null 2>&1; then
  fail "missing preparation command must fail"
fi

printf 'ok - Worker root preparation is geometry-based and idempotent\n'
