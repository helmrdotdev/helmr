#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/dev/aws/run-auth-readiness.sh"

fail() { printf 'not ok - %s\n' "$1" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

cat >"${tmp}/preflight" <<'EOF'
#!/usr/bin/env bash
[ "$1" = "--setup-only" ]
EOF
cat >"${tmp}/helmr" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "project list --json") printf '{"projects":[{"id":"private","slug":"helmr"}]}\n' ;;
  "env list --project helmr --json") printf '[{"id":"private-a","slug":"staging"},{"id":"private-b","slug":"production"}]\n' ;;
  *) exit 2 ;;
esac
EOF
chmod +x "${tmp}/preflight" "${tmp}/helmr"

result="${tmp}/result.json"
HELMR_VALIDATION_RESULT_FILE="${result}" \
HELMR_AUTH_PREFLIGHT_BIN="${tmp}/preflight" \
HELMR_BIN="${tmp}/helmr" \
HELMR_API_URL="https://example.invalid" \
HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 \
  "${script}"

[ "$(jq -r '.status' "${result}")" = "passed" ] || fail "auth readiness should pass"
[ "$(jq -r '.observations.project_slug' "${result}")" = "helmr" ] || fail "sanitized project slug"
if rg -q 'private-' "${result}"; then
  fail "auth readiness result must not persist identifiers"
fi

printf 'ok - auth readiness tests\n'
