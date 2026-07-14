#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$repo_root/scripts/aws-dev-debug.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  file=$1
  expected=$2
  label=$3
  grep -Fq -- "$expected" "$file" || fail "$label: missing $expected"
}

mkdir -p "$tmp/bin"
cat >"$tmp/bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

output=
while [ "$#" -gt 0 ]; do
  case "$1" in
    output) shift; [ "${1:-}" = "-raw" ] && shift; output=${1:-}; break ;;
    *) shift ;;
  esac
done

[ "${MOCK_TOFU_FAIL:-0}" != "1" ] || exit 42
case "$output" in
  worker_autoscaling_group_name) printf '%s\n' "${MOCK_RUN_ASG:-null}" ;;
  build_worker_autoscaling_group_name) printf '%s\n' "${MOCK_BUILD_ASG:-null}" ;;
  *) printf 'null\n' ;;
esac
EOF
chmod +x "$tmp/bin/tofu"

stdout="$tmp/stdout"
stderr="$tmp/stderr"

"$script" help >"$stdout" 2>"$stderr"
if grep -Eq 'worker-(up|down)' "$stdout"; then
  fail "help must not advertise manual managed-worker scaling"
fi

if "$script" worker-up >"$stdout" 2>"$stderr"; then
  fail "worker-up compatibility command must be absent"
fi
assert_contains "$stderr" "unknown command: worker-up" "manual worker scaling rejection"

if MOCK_RUN_ASG=helmr-smoke-run-worker \
  TF_BIN="$tmp/bin/tofu" \
  "$script" dev-off >"$stdout" 2>"$stderr"; then
  fail "dev-off must reject a managed-worker topology"
fi
assert_contains "$stderr" "operation cannot bypass application-owned worker drain" "managed worker dev-off guard"

if MOCK_RUN_ASG=helmr-smoke-run-worker \
  TF_BIN="$tmp/bin/tofu" \
  "$script" control-down >"$stdout" 2>"$stderr"; then
  fail "control-down must reject a managed-worker topology"
fi
assert_contains "$stderr" "operation cannot bypass application-owned worker drain" "managed worker control-down guard"

if MOCK_RUN_ASG=helmr-smoke-run-worker \
  TF_BIN="$tmp/bin/tofu" \
  "$script" control-up 1 0 >"$stdout" 2>"$stderr"; then
  fail "control-up must not stop the dispatcher on a managed-worker topology"
fi
assert_contains "$stderr" "operation cannot bypass application-owned worker drain" "managed worker dispatcher guard"

if MOCK_RUN_ASG=helmr-smoke-run-worker \
  TF_BIN="$tmp/bin/tofu" \
  "$script" database-down >"$stdout" 2>"$stderr"; then
  fail "database-down must reject a managed-worker topology"
fi
assert_contains "$stderr" "operation cannot bypass application-owned worker drain" "managed worker database-down guard"

if MOCK_TOFU_FAIL=1 \
  TF_BIN="$tmp/bin/tofu" \
  "$script" dev-off >"$stdout" 2>"$stderr"; then
  fail "dev-off must fail closed when topology cannot be proved"
fi
assert_contains "$stderr" "cannot prove that the stack is control-only" "dev-off topology proof guard"

printf 'ok - aws dev debug tests\n'
