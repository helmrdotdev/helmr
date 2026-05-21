#!/usr/bin/env bash
set -euo pipefail

tf="${TOFU:-tofu}"
overwrite="${OVERWRITE_SECRETS:-0}"

secret_arns="$("$tf" output -json secret_arns)"

secret_arn() {
  jq -er --arg key "$1" '.[$key]' <<<"$secret_arns"
}

secret_has_value() {
  aws secretsmanager get-secret-value \
    --secret-id "$1" \
    --query SecretString \
    --output text >/dev/null 2>&1
}

put_secret() {
  local key="$1"
  local value="$2"
  local arn

  arn="$(secret_arn "$key")"
  if [ "$overwrite" != "1" ] && secret_has_value "$arn"; then
    printf 'skip %s: already has AWSCURRENT value\n' "$key" >&2
    return 0
  fi

  aws secretsmanager put-secret-value \
    --secret-id "$arn" \
    --secret-string "$value" >/dev/null
  printf 'populated %s\n' "$key" >&2
}

put_secret_file() {
  local key="$1"
  local path="$2"
  local arn

  arn="$(secret_arn "$key")"
  if [ "$overwrite" != "1" ] && secret_has_value "$arn"; then
    printf 'skip %s: already has AWSCURRENT value\n' "$key" >&2
    return 0
  fi

  aws secretsmanager put-secret-value \
    --secret-id "$arn" \
    --secret-string "file://${path}" >/dev/null
  printf 'populated %s\n' "$key" >&2
}

random_base64_32() {
  openssl rand -base64 32 | tr -d '\n'
}

put_secret worker_token_signing_key "$(openssl rand -hex 32)"
put_secret auth_secret "$(openssl rand -hex 32)"
put_secret secret_encryption_key "$(random_base64_32)"
put_secret checkpoint_encryption_key "$(random_base64_32)"
put_secret worker_bootstrap_token "$(openssl rand -hex 32)"
put_secret setup_token "$(openssl rand -hex 32)"

if [ -n "${HELMR_DATABASE_URL:-}" ]; then
  put_secret database_url "$HELMR_DATABASE_URL"
fi

if [ -n "${HELMR_GITHUB_APP_PRIVATE_KEY_FILE:-}" ]; then
  put_secret_file github_app_private_key "$HELMR_GITHUB_APP_PRIVATE_KEY_FILE"
elif [ -n "${HELMR_GITHUB_APP_PRIVATE_KEY:-}" ]; then
  put_secret github_app_private_key "$HELMR_GITHUB_APP_PRIVATE_KEY"
fi

if [ -n "${HELMR_GITHUB_APP_WEBHOOK_SECRET:-}" ]; then
  put_secret github_app_webhook_secret "$HELMR_GITHUB_APP_WEBHOOK_SECRET"
fi

if [ -n "${HELMR_GITHUB_APP_CLIENT_SECRET:-}" ]; then
  put_secret github_app_client_secret "$HELMR_GITHUB_APP_CLIENT_SECRET"
fi
