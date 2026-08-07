#!/usr/bin/env bash
set -euo pipefail

tf="${TOFU:-tofu}"

secret_arns="$("$tf" output -json secret_arns)"
worker_enrollment_secret_arn="$("$tf" output -raw worker_enrollment_secret_arn)"

secret_arn() {
  jq -er --arg key "$1" '.[$key]' <<<"$secret_arns"
}

secret_arn_optional() {
  jq -r --arg key "$1" '.[$key] // empty' <<<"$secret_arns"
}

secret_value_status() {
  local arn="$1"
  local error_file

  error_file="$(mktemp)"
  if aws secretsmanager get-secret-value \
    --secret-id "$arn" \
    --query SecretString \
    --output text >/dev/null 2>"$error_file"; then
    rm -f "$error_file"
    printf 'present\n'
    return 0
  fi
  if grep -q 'ResourceNotFoundException' "$error_file"; then
    rm -f "$error_file"
    printf 'missing\n'
    return 0
  fi
  cat "$error_file" >&2
  rm -f "$error_file"
  return 1
}

put_secret_arn() {
  local label="$1"
  local arn="$2"
  local value="$3"
  local status

  status="$(secret_value_status "$arn")"
  case "$status" in
    present)
      printf 'skip %s: already has AWSCURRENT value\n' "$label" >&2
      return 0
      ;;
    missing) ;;
    *)
      printf 'unexpected secret status for %s: %s\n' "$label" "$status" >&2
      return 1
      ;;
  esac
  aws secretsmanager put-secret-value --secret-id "$arn" --secret-string "$value" >/dev/null
  printf 'populated %s\n' "$label" >&2
}

put_secret() {
  local key="$1"
  local value="$2"

  put_secret_arn "$key" "$(secret_arn "$key")" "$value"
}

random_base64_32() {
  openssl rand -base64 32 | tr -d '\n'
}

random_base64url_32() {
  openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'
}

put_secret worker_token_signing_key "$(random_base64_32)"
put_secret auth_key "$(random_base64_32)"
put_secret encryption_key "$(random_base64_32)"
put_secret workspace_fencing_key "$(random_base64_32)"
put_secret token_credential_key "$(random_base64_32)"
put_secret checkpoint_encryption_key "$(random_base64_32)"
setup_token_secret_arn="$(secret_arn_optional setup_token)"
if [ -n "$setup_token_secret_arn" ]; then
  put_secret_arn setup_token "$setup_token_secret_arn" "$(openssl rand -hex 32)"
fi

if [ -n "$worker_enrollment_secret_arn" ]; then
  put_secret_arn "worker_enrollment_token" "$worker_enrollment_secret_arn" "hlmr_wgt_$(random_base64url_32)"
fi

if [ -n "${HELMR_DATABASE_URL:-}" ]; then
  put_secret database_url "$HELMR_DATABASE_URL"
fi

if [ -n "${HELMR_GITHUB_OAUTH_CLIENT_SECRET:-}" ]; then
  put_secret github_oauth_client_secret "$HELMR_GITHUB_OAUTH_CLIENT_SECRET"
fi
