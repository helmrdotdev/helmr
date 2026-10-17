#!/usr/bin/env bash
set -euo pipefail
cd /workspace/native-sdk
mkdir -p isolated-startup/home
# Diagnose loading separately from behavior. This intentionally warms package
# files; subsequent case results therefore do not establish cold-start latency.
exec timeout --kill-after=5s 480s unshare --fork --pid --mount-proc --net /usr/bin/tini -s -- sh -ceu '
  ip link set lo up
  exec env -i PATH="$PWD/node_modules/@oven/bun-linux-x64/bin:/usr/local/bin:/usr/bin:/bin" HOME="$PWD/isolated-startup/home" TMPDIR=/tmp bash -s
' <<'STEPS'
set -u
step() {
  local label=$1 status started=$SECONDS
  shift
  printf 'STARTUP_BEGIN %s %s\n' "$label" "$(date -u +%FT%TZ)"
  timeout --kill-after=2s 120s "$@" </dev/null
  status=$?
  printf 'STARTUP_END %s exit=%s elapsed_seconds=%s %s\n' "$label" "$status" "$((SECONDS-started))" "$(date -u +%FT%TZ)"
}
for family in bun codex claude pi opencode; do
  for pass in 1 2; do
    case "$family" in
      bun)
        step "$pass-bun-read" sh -c 'find node_modules/@oven/bun-linux-x64 -type f -exec cat {} + >/dev/null'
        step "$pass-bun-version" bun --version
        ;;
      codex)
        step "$pass-codex-read" sh -c 'find node_modules/@openai/codex-linux-x64 -type f -exec cat {} + >/dev/null'
        step "$pass-codex-version" node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex --version
        ;;
      claude)
        step "$pass-claude-read" sh -c 'find node_modules/@anthropic-ai/claude-agent-sdk node_modules/@anthropic-ai/claude-agent-sdk-linux-x64 -type f -exec cat {} + >/dev/null'
        step "$pass-claude-import" bun -e 'await import("@anthropic-ai/claude-agent-sdk"); console.log("import ready")'
        step "$pass-claude-version" node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude --version
        ;;
      pi)
        step "$pass-pi-read" sh -c 'find node_modules/@earendil-works/pi-ai node_modules/@earendil-works/pi-coding-agent -type f -exec cat {} + >/dev/null'
        step "$pass-pi-import" bun -e 'await import("@earendil-works/pi-ai"); await import("@earendil-works/pi-coding-agent"); console.log("import ready")'
        ;;
      opencode)
        step "$pass-opencode-read" sh -c 'find node_modules/@opencode/sdk node_modules/@opencode/core node_modules/effect -type f -exec cat {} + >/dev/null'
        step "$pass-opencode-import" bun -e 'for (const name of ["@opencode/sdk/effect", "@opencode/core/session/execution/restart", "@opencode/core/session/execution", "@opencode/core/session/store", "@opencode/core/session/inbox", "@opencode/core/session/message", "@opencode/core/session", "@opencode/core/job", "effect"]) await import(name); console.log("import ready")'
        ;;
    esac
  done
done
printf 'STARTUP_COMPLETE\n'
STEPS
