#!/usr/bin/env bash
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  printf 'runtime release verification must run as root\n' >&2
  exit 1
fi
if [ "$#" -lt 2 ]; then
  printf 'usage: scripts/run-runtime-release.sh <runtime-release-binary> <command> [arguments...]\n' >&2
  exit 1
fi

cgroup_relative="$(
  awk -F: '$1 == "0" { print $3; found = 1 } END { if (!found) exit 1 }' /proc/self/cgroup
)"
cgroup_root="/sys/fs/cgroup${cgroup_relative}"
[ -d "${cgroup_root}" ] || {
  printf 'runtime release verifier cgroup does not exist: %s\n' "${cgroup_root}" >&2
  exit 1
}
[ -w "${cgroup_root}/cgroup.procs" ] || {
  printf 'runtime release verifier cgroup is not delegated: %s\n' "${cgroup_root}" >&2
  exit 1
}

binary="$1"
shift
exec "${binary}" "$@" --cgroup-root "${cgroup_root}"
