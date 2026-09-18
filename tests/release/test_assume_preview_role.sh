#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/../.." && pwd)
script="$repo_root/scripts/release/assume-preview-role.sh"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

test_exports_masked_credentials_with_request_token() {
  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  cat >"$tmp/bin/curl" <<'SH'
#!/usr/bin/env sh
printf '{"value":"oidc-token-value"}\n'
SH
  cat >"$tmp/bin/aws" <<'SH'
#!/usr/bin/env sh
printf 'AKIAFAKE\tsecret\tsession\n'
SH
  chmod +x "$tmp/bin/curl" "$tmp/bin/aws"
  PATH="$tmp/bin:$PATH" \
    ACTIONS_ID_TOKEN_REQUEST_TOKEN='request-token' \
    ACTIONS_ID_TOKEN_REQUEST_URL='https://example.invalid/token' \
    PREVIEW_PUBLISHER_ROLE_ARN='arn:aws:iam::123:role/preview' \
    GITHUB_RUN_ID='99' \
    GITHUB_ENV="$tmp/env" \
    bash "$script" >"$tmp/log" 2>&1
  grep -F '::add-mask::oidc-token-value' "$tmp/log" >/dev/null || fail 'OIDC token not masked'
  grep -F 'AWS_ACCESS_KEY_ID=AKIAFAKE' "$tmp/env" >/dev/null || fail 'AWS key not exported'
  grep -F 'AWS_DEFAULT_REGION=us-east-1' "$tmp/env" >/dev/null || fail 'region not exported'
  rm -rf "$tmp"
}

test_rejects_wrong_oidc_env_name() {
  local tmp
  tmp="$(mktemp -d)"
  if ACTIONS_ID_TOKEN='wrong-name' \
    ACTIONS_ID_TOKEN_REQUEST_URL='https://example.invalid/token' \
    PREVIEW_PUBLISHER_ROLE_ARN='arn:aws:iam::123:role/preview' \
    GITHUB_RUN_ID='99' \
    GITHUB_ENV="$tmp/env" \
    bash "$script" >/dev/null 2>&1; then
    rm -rf "$tmp"
    fail 'wrong OIDC env name must fail under set -u'
  fi
  rm -rf "$tmp"
}

test_sts_failure_is_not_hidden() {
  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  cat >"$tmp/bin/curl" <<'SH'
#!/usr/bin/env sh
printf '{"value":"oidc-token-value"}\n'
SH
  cat >"$tmp/bin/aws" <<'SH'
#!/usr/bin/env sh
echo 'STS failed' >&2
exit 42
SH
  chmod +x "$tmp/bin/curl" "$tmp/bin/aws"
  set +e
  PATH="$tmp/bin:$PATH" \
    ACTIONS_ID_TOKEN_REQUEST_TOKEN='request-token' \
    ACTIONS_ID_TOKEN_REQUEST_URL='https://example.invalid/token' \
    PREVIEW_PUBLISHER_ROLE_ARN='arn:aws:iam::123:role/preview' \
    GITHUB_RUN_ID='99' \
    GITHUB_ENV="$tmp/env" \
    bash "$script" >"$tmp/out" 2>"$tmp/err"
  status=$?
  set -e
  [ "$status" -eq 1 ] || { rm -rf "$tmp"; fail "expected STS exit 1, got $status"; }
  grep -F 'preview publisher STS assume-role failed' "$tmp/err" >/dev/null || { rm -rf "$tmp"; fail 'STS failure message missing'; }
  rm -rf "$tmp"
}

test_github_env_not_visible_in_same_step_shell() {
  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  cat >"$tmp/bin/curl" <<'SH'
#!/usr/bin/env sh
printf '{"value":"oidc-token-value"}\n'
SH
  cat >"$tmp/bin/aws" <<'SH'
#!/usr/bin/env sh
printf 'AKIAFAKE\tsecret\tsession\n'
SH
  chmod +x "$tmp/bin/curl" "$tmp/bin/aws"
  set +e
  same_step=$(
    PATH="$tmp/bin:$PATH" \
      ACTIONS_ID_TOKEN_REQUEST_TOKEN='request-token' \
      ACTIONS_ID_TOKEN_REQUEST_URL='https://example.invalid/token' \
      PREVIEW_PUBLISHER_ROLE_ARN='arn:aws:iam::123:role/preview' \
      GITHUB_RUN_ID='99' \
      GITHUB_ENV="$tmp/env" \
      bash -c "bash \"$script\" >/dev/null; printf '%s' \"\${AWS_ACCESS_KEY_ID:-UNSET}\""
  )
  set -e
  [ "$same_step" = 'UNSET' ] || { rm -rf "$tmp"; fail "same-step shell must not inherit GITHUB_ENV exports (got $same_step)"; }
  rm -rf "$tmp"
}

test_github_env_visible_in_next_step_process() {
  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "$tmp/bin"
  cat >"$tmp/bin/curl" <<'SH'
#!/usr/bin/env sh
printf '{"value":"oidc-token-value"}\n'
SH
  cat >"$tmp/bin/aws" <<'SH'
#!/usr/bin/env sh
printf 'AKIAFAKE\tsecret\tsession\n'
SH
  chmod +x "$tmp/bin/curl" "$tmp/bin/aws"
  PATH="$tmp/bin:$PATH" \
    ACTIONS_ID_TOKEN_REQUEST_TOKEN='request-token' \
    ACTIONS_ID_TOKEN_REQUEST_URL='https://example.invalid/token' \
    PREVIEW_PUBLISHER_ROLE_ARN='arn:aws:iam::123:role/preview' \
    GITHUB_RUN_ID='99' \
    GITHUB_ENV="$tmp/env" \
    bash "$script" >/dev/null
  next_step=$(
    set -a
    # shellcheck disable=SC1091
    . "$tmp/env"
    set +a
    printf '%s' "${AWS_ACCESS_KEY_ID:-UNSET}"
  )
  [ "$next_step" = 'AKIAFAKE' ] || fail "next-step process must see AWS_ACCESS_KEY_ID from GITHUB_ENV (got $next_step)"
  rm -rf "$tmp"
}

test_exports_masked_credentials_with_request_token
test_rejects_wrong_oidc_env_name
test_sts_failure_is_not_hidden
test_github_env_not_visible_in_same_step_shell
test_github_env_visible_in_next_step_process
printf 'ok - assume preview role tests\n'
