#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=tests/buildkit-steps.sh
source "$repo_root/tests/buildkit-steps.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Two graphs reuse step numbers: #5 ran in the first and is CACHED in the second.
cat >"$tmp/build.log" <<'LOG'
#0 building with "helmr-test" instance using docker-container driver
#4 [environment 2/3] COPY [/input.txt,/etc/input]
#4 CACHED
#5 [environment 3/3] RUN ["/bin/sh","/opt/setup.sh"]
#5 0.412 preparing
#5 DONE 1.2s
#0 building with "helmr-test" instance using docker-container driver
#5 [installed 1/4] WORKDIR /workspace/project
#5 CACHED
#9 [installed 4/4] RUN ["npm","ci"]
#9 CACHED
LOG

status() { local code=0; "$@" >/dev/null 2>&1 || code=$?; printf '%s' "$code"; }
expect() {
  local want="$1"; shift
  local got; got=$(status "$@")
  [ "$got" = "$want" ] || { echo "not ok - $* returned $got, want $want" >&2; exit 1; }
}

expect 0 step_cached "$tmp/build.log" 'COPY [/input.txt,/etc/input]'
expect 0 step_cached "$tmp/build.log" 'RUN ["npm","ci"]'
# The other graph's "#5 CACHED" does not make this step cached.
expect 1 step_cached "$tmp/build.log" 'RUN ["/bin/sh","/opt/setup.sh"]'
expect 2 step_cached "$tmp/build.log" 'RUN ["/bin/sh","/opt/renamed.sh"]'

expect 0 step_ran "$tmp/build.log" 'RUN ["/bin/sh","/opt/setup.sh"]'
expect 1 step_ran "$tmp/build.log" 'RUN ["npm","ci"]'
# A step that is not in the log is an error, not proof that it reran.
expect 2 step_ran "$tmp/build.log" 'RUN ["/bin/sh","/opt/renamed.sh"]'

printf 'ok - BuildKit step cache parsing\n'
