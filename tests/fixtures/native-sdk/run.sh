#!/usr/bin/env bash
set -euo pipefail
cd /workspace/native-sdk
[[ $(id -u) == 0 ]]
mkdir -p evidence
cases=(claude-native-probe.mjs claude-native-dirty-reopen.mjs claude-native-close-tool.mjs pi-queue-recovery.ts pi-native-tool.ts pi-tool-recovery.ts opencode-recovery-final.ts opencode-interrupt-parent.ts opencode-crash-parent.ts codex-lifecycle.ts codex-controls-baseline.ts codex-tools.ts codex-crash-baseline.ts codex-goals-disabled.ts)
failed=0
for case in "${cases[@]}"; do
  rm -rf isolated
  mkdir -p isolated/home
  started=$SECONDS
  printf 'CASE %s\n' "$case"
  printf 'PHASE %s start %s\n' "$case" "$(date -u +%FT%TZ)"
  # Each namespace dies before the next case, including timeout/failure paths.
  if timeout --kill-after=5s 55s unshare --fork --pid --mount-proc --net /usr/bin/tini -s -- sh -ceu '
    ip link set lo up
    exec env -i PATH="$PWD/node_modules/@oven/bun-linux-x64/bin:/usr/local/bin:/usr/bin:/bin" HOME="$PWD/isolated/home" TMPDIR=/tmp node_modules/@oven/bun-linux-x64/bin/bun "$1"
  ' sh "$case" > "evidence/$case.log" 2>&1; then
    cat "evidence/$case.log"
    printf 'DIAGNOSTIC %s exit=0 elapsed_seconds=%s\n' "$case" "$((SECONDS-started))"
    printf 'PASS %s\n' "$case"
  else
    status=$?
    cat "evidence/$case.log"
    printf 'DIAGNOSTIC %s exit=%s elapsed_seconds=%s\n' "$case" "$status" "$((SECONDS-started))"
    printf 'FAIL %s\n' "$case"
    failed=1
  fi
done
exit "$failed"
