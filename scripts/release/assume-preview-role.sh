#!/usr/bin/env bash
# Assume the preview publisher OIDC role. Invoke from nix develop .#release -c bash scripts/release/assume-preview-role.sh
set -euo pipefail
: "${PREVIEW_PUBLISHER_ROLE_ARN:?}"
: "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:?}"
: "${ACTIONS_ID_TOKEN_REQUEST_URL:?}"
: "${GITHUB_RUN_ID:?}"
: "${GITHUB_ENV:?}"

token=$(curl -fsSL -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
  "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=sts.amazonaws.com" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["value"])')
echo "::add-mask::$token"

if ! creds=$(aws sts assume-role-with-web-identity \
  --role-arn "$PREVIEW_PUBLISHER_ROLE_ARN" \
  --role-session-name "release-${GITHUB_RUN_ID}" \
  --web-identity-token "$token" \
  --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' \
  --output text); then
  echo 'preview publisher STS assume-role failed' >&2
  exit 1
fi
read -r id secret session <<<"$creds"
for value in "$id" "$secret" "$session"; do
  echo "::add-mask::$value"
done
{
  echo "AWS_ACCESS_KEY_ID=$id"
  echo "AWS_SECRET_ACCESS_KEY=$secret"
  echo "AWS_SESSION_TOKEN=$session"
  echo "AWS_DEFAULT_REGION=us-east-1"
} >> "$GITHUB_ENV"
