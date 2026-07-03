#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OP_ENV_FILE="${OP_ENV_FILE:-${HOME}/.config/helmr/1password/helmr-dev.env}"
OP_VAULT="${OP_VAULT:-helmr-dev}"

usage() {
  cat <<'EOF'
Usage: scripts/dev-secrets.sh <command> [args...]

Commands:
  init-env-file        Create a local 1Password reference env file template.
  check                Verify op is available and the env file exists.
  run -- <command>     Run a command with 1Password references injected.
  aws-dev-smoke ...    Run scripts/aws-dev-smoke.sh with 1Password references injected.

Environment:
  OP_ENV_FILE          1Password reference env file. Defaults to
                       ~/.config/helmr/1password/helmr-dev.env.
  OP_VAULT             1Password vault name used by init-env-file references.
                       Defaults to helmr-dev.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_op() {
  command -v op >/dev/null 2>&1 || die "missing op; run from nix develop .#infra"
}

init_env_file() {
  if [ -e "${OP_ENV_FILE}" ]; then
    die "${OP_ENV_FILE} already exists"
  fi
  mkdir -p "$(dirname "${OP_ENV_FILE}")"
  chmod 700 "$(dirname "${OP_ENV_FILE}")"
  {
    printf '# Helmr dev secret references. This file stores op:// references only, not secret values.\n'
    printf '# Keep this file outside the repo and run commands through scripts/dev-secrets.sh.\n'
    printf '\n'
    printf 'DEV_CREATE_CLICKHOUSE_CLOUD=true\n'
    printf 'DEV_CLICKHOUSE_ORGANIZATION_ID=op://%s/clickhouse-cloud-terraform-dev/organization_id\n' "${OP_VAULT}"
    printf 'CLICKHOUSE_CLOUD_API_KEY=op://%s/clickhouse-cloud-terraform-dev/api_key\n' "${OP_VAULT}"
    printf 'CLICKHOUSE_CLOUD_API_SECRET=op://%s/clickhouse-cloud-terraform-dev/api_secret\n' "${OP_VAULT}"
    printf '\n'
    printf 'DEV_GITHUB_OAUTH_CLIENT_ID=op://%s/github-oauth-dev/client_id\n' "${OP_VAULT}"
    printf 'HELMR_GITHUB_OAUTH_CLIENT_SECRET=op://%s/github-oauth-dev/client_secret\n' "${OP_VAULT}"
    printf '\n'
    printf '# Optional external provider references can be added here when a smoke path needs them.\n'
    printf '# RESEND_API_KEY=op://%s/resend-dev/api_key\n' "${OP_VAULT}"
    printf '# ANTHROPIC_API_KEY=op://%s/anthropic-dev/api_key\n' "${OP_VAULT}"
    printf '# ZEROENTROPY_API_KEY=op://%s/zeroentropy-dev/api_key\n' "${OP_VAULT}"
  } >"${OP_ENV_FILE}"
  chmod 600 "${OP_ENV_FILE}"
  printf 'created %s\n' "${OP_ENV_FILE}" >&2
}

check_env() {
  need_op
  [ -f "${OP_ENV_FILE}" ] || die "missing ${OP_ENV_FILE}; run scripts/dev-secrets.sh init-env-file"
  op account list >/dev/null
  # shellcheck disable=SC2016
  op run --env-file "${OP_ENV_FILE}" -- sh -eu -c '
    : "${DEV_CREATE_CLICKHOUSE_CLOUD:?}"
    : "${DEV_CLICKHOUSE_ORGANIZATION_ID:?}"
    : "${CLICKHOUSE_CLOUD_API_KEY:?}"
    : "${CLICKHOUSE_CLOUD_API_SECRET:?}"
    : "${DEV_GITHUB_OAUTH_CLIENT_ID:?}"
    : "${HELMR_GITHUB_OAUTH_CLIENT_SECRET:?}"
  '
  printf 'op ready; using %s\n' "${OP_ENV_FILE}" >&2
}

run_with_secrets() {
  need_op
  [ -f "${OP_ENV_FILE}" ] || die "missing ${OP_ENV_FILE}; run scripts/dev-secrets.sh init-env-file"
  op run --env-file "${OP_ENV_FILE}" -- "$@"
}

command="${1:-}"
case "${command}" in
  init-env-file)
    shift
    [ "$#" -eq 0 ] || die "init-env-file does not accept arguments"
    init_env_file
    ;;
  check)
    shift
    [ "$#" -eq 0 ] || die "check does not accept arguments"
    check_env
    ;;
  run)
    shift
    [ "${1:-}" = "--" ] || die "run requires -- before the command"
    shift
    [ "$#" -gt 0 ] || die "run requires a command"
    run_with_secrets "$@"
    ;;
  aws-dev-smoke)
    shift
    [ "$#" -gt 0 ] || die "aws-dev-smoke requires a scripts/aws-dev-smoke.sh command"
    run_with_secrets "${ROOT}/scripts/aws-dev-smoke.sh" "$@"
    ;;
  ""|-h|--help|help)
    usage
    ;;
  *)
    usage >&2
    exit 1
    ;;
esac
