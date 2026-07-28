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
  local value

  arn="$(secret_arn "$key")"
  if [ "$overwrite" != "1" ] && secret_has_value "$arn"; then
    printf 'skip %s: already has AWSCURRENT value\n' "$key" >&2
    return 0
  fi

  value="$(<"$path")"
  aws secretsmanager put-secret-value \
    --secret-id "$arn" \
    --secret-string "$value" >/dev/null
  printf 'populated %s\n' "$key" >&2
}

random_base64_32() {
  openssl rand -base64 32 | tr -d '\n'
}

read_secret() {
  aws secretsmanager get-secret-value \
    --secret-id "$1" \
    --query SecretString \
    --output text
}

workspace_fencing_fingerprint() {
  {
    printf 'helmr.workspace-fence-key.v0\0'
    printf '%s' "$1" | openssl base64 -d -A
  } | openssl dgst -sha256 -r | awk '{ print "sha256:" $1 }'
}

token_credential_key_id() {
  {
    printf 'helmr.token-credential-key.v0\0'
    printf '%s' "$1" | openssl base64 -d -A
  } | openssl dgst -sha256 -r | awk '{ print "sha256:" $1 }'
}

validate_base64_32_key() {
  local encoded="$1"
  local canonical
  local size

  canonical="$(printf '%s' "$encoded" | openssl base64 -d -A | openssl base64 -A)"
  if [ "$canonical" != "$encoded" ]; then
    printf 'key is not canonical base64\n' >&2
    return 1
  fi
  size="$(
    printf '%s' "$encoded" |
      openssl base64 -d -A |
      wc -c |
      tr -d '[:space:]'
  )"
  if [ "$size" != "32" ]; then
    printf 'key must decode to exactly 32 bytes\n' >&2
    return 1
  fi
}

initialize_workspace_fencing_authority() {
  local selected="${HELMR_WORKSPACE_FENCING_KEY_FINGERPRINT:-}"
  local keys_arn
  local keys
  local encoded
  local fingerprint
  local entry_fingerprint

  keys_arn="$(secret_arn workspace_fencing_keys)"
  if secret_has_value "$keys_arn"; then
    keys="$(read_secret "$keys_arn")"
  else
    encoded="$(random_base64_32)"
    fingerprint="$(workspace_fencing_fingerprint "$encoded")"
    keys="{\"${fingerprint}\":\"${encoded}\"}"
    aws secretsmanager put-secret-value \
      --secret-id "$keys_arn" \
      --secret-string "$keys" >/dev/null
    printf 'populated workspace_fencing_keys\n' >&2
  fi

  jq -e '
    type == "object" and length > 0 and
    all(keys[]; test("^sha256:[0-9a-f]{64}$")) and
    all(.[]; type == "string")
  ' <<<"$keys" >/dev/null
  while IFS= read -r entry_fingerprint; do
    encoded="$(jq -er --arg fingerprint "$entry_fingerprint" '.[$fingerprint]' <<<"$keys")"
    validate_base64_32_key "$encoded"
    fingerprint="$(workspace_fencing_fingerprint "$encoded")"
    if [ "$entry_fingerprint" != "$fingerprint" ]; then
      printf 'workspace fencing key %s does not match its content fingerprint\n' "$entry_fingerprint" >&2
      return 1
    fi
  done < <(jq -r 'keys[]' <<<"$keys")

  if [ -n "$selected" ]; then
    jq -e --arg fingerprint "$selected" 'has($fingerprint)' <<<"$keys" >/dev/null ||
      {
        printf 'selected Workspace fencing key %s is not present\n' "$selected" >&2
        return 1
      }
    fingerprint="$selected"
  elif [ "$(jq 'length' <<<"$keys")" -eq 1 ]; then
    fingerprint="$(jq -er 'keys[0]' <<<"$keys")"
  else
    printf 'HELMR_WORKSPACE_FENCING_KEY_FINGERPRINT is required when multiple Workspace fencing keys are readable\n' >&2
    return 1
  fi

  printf 'set workspace_fencing_key_fingerprint = "%s" before enabling the Control service\n' "$fingerprint" >&2
}

initialize_token_credential_authority() {
  local selected="${HELMR_TOKEN_CREDENTIAL_KEY_ID:-}"
  local keys_arn
  local keys
  local encoded
  local key_id
  local entry_key_id

  keys_arn="$(secret_arn token_credential_keys)"
  if secret_has_value "$keys_arn"; then
    keys="$(read_secret "$keys_arn")"
  else
    encoded="$(random_base64_32)"
    key_id="$(token_credential_key_id "$encoded")"
    keys="{\"${key_id}\":\"${encoded}\"}"
    aws secretsmanager put-secret-value \
      --secret-id "$keys_arn" \
      --secret-string "$keys" >/dev/null
    printf 'populated token_credential_keys\n' >&2
  fi

  jq -e '
    type == "object" and length > 0 and
    all(keys[]; test("^sha256:[0-9a-f]{64}$")) and
    all(.[]; type == "string")
  ' <<<"$keys" >/dev/null
  while IFS= read -r entry_key_id; do
    encoded="$(jq -er --arg key_id "$entry_key_id" '.[$key_id]' <<<"$keys")"
    validate_base64_32_key "$encoded"
    key_id="$(token_credential_key_id "$encoded")"
    if [ "$entry_key_id" != "$key_id" ]; then
      printf 'Token credential key %s does not match its content-derived key ID\n' "$entry_key_id" >&2
      return 1
    fi
  done < <(jq -r 'keys[]' <<<"$keys")

  if [ -n "$selected" ]; then
    jq -e --arg key_id "$selected" 'has($key_id)' <<<"$keys" >/dev/null ||
      {
        printf 'selected Token credential key %s is not present\n' "$selected" >&2
        return 1
      }
    key_id="$selected"
  elif [ "$(jq 'length' <<<"$keys")" -eq 1 ]; then
    key_id="$(jq -er 'keys[0]' <<<"$keys")"
  else
    printf 'HELMR_TOKEN_CREDENTIAL_KEY_ID is required when multiple Token credential keys are readable\n' >&2
    return 1
  fi

  printf 'set token_credential_key_id = "%s" before enabling the Control service\n' "$key_id" >&2
}

put_secret worker_token_signing_key "$(openssl rand -hex 32)"
put_secret auth_secret "$(openssl rand -hex 32)"
put_secret encryption_key "$(random_base64_32)"
initialize_workspace_fencing_authority
initialize_token_credential_authority
put_secret checkpoint_encryption_key "$(random_base64_32)"
put_secret setup_token "$(openssl rand -hex 32)"

if [ -n "${HELMR_DATABASE_URL:-}" ]; then
  put_secret database_url "$HELMR_DATABASE_URL"
fi

if [ -n "${HELMR_GITHUB_OAUTH_CLIENT_SECRET:-}" ]; then
  put_secret github_oauth_client_secret "$HELMR_GITHUB_OAUTH_CLIENT_SECRET"
fi
