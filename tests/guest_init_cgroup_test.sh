#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
guest_init="${repo_root}/images/guest/init.sh"

line_number() {
  local text=$1
  local line
  line="$(sed 's/^[[:space:]]*//' "${guest_init}" | grep -nFx -- "${text}" | cut -d: -f1)"
  if [[ ! "${line}" =~ ^[0-9]+$ ]]; then
    printf 'expected exactly one guest init contract line: %s\n' "${text}" >&2
    exit 1
  fi
  printf '%s\n' "${line}"
}

if grep -Fq 'helmr.pids_max' "${guest_init}" ||
  grep -Fq 'configure_process_limit' "${guest_init}"; then
  printf 'retired managed-build PID policy remains in guest init\n' >&2
  exit 1
fi

mount_line="$(line_number 'is_mounted /sys/fs/cgroup || mount -t cgroup2 cgroup2 /sys/fs/cgroup')"
parent_line="$(line_number 'mkdir /sys/fs/cgroup/helmr')"
supervisor_line="$(line_number 'mkdir /sys/fs/cgroup/helmr/supervisor')"
move_line="$(line_number 'echo $$ > /sys/fs/cgroup/helmr/supervisor/cgroup.procs')"
empty_line="$(line_number 'if [ -n "$(cat /sys/fs/cgroup/helmr/cgroup.procs)" ]; then')"
identity_line="$(line_number "if ! grep -qx '0::/helmr/supervisor' /proc/self/cgroup; then")"
bootstrap_line="$(line_number 'configure_program_cgroups')"
guestd_line="$(line_number "exec /usr/bin/guestd \\")"

if ! ((
  mount_line < parent_line &&
  parent_line < supervisor_line &&
  supervisor_line < move_line &&
  move_line < empty_line &&
  empty_line < identity_line &&
  bootstrap_line < guestd_line
)); then
  printf 'guest init Program cgroup bootstrap order is invalid\n' >&2
  exit 1
fi

if grep -Eq 'cgroup\.subtree_control|pids\.max|\+cpu|\+memory|\+pids' "${guest_init}"; then
  printf 'guest init Program cgroups unexpectedly define a resource controller policy\n' >&2
  exit 1
fi

bash -n "${guest_init}"
printf 'ok - guest init Program cgroup contract\n'
