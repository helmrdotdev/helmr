#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/aws-bootstrap-helmr-secrets.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/bin"

cat >"${tmp}/bin/mock-tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'output -json secret_arns')
    jq -n --arg setup "${MOCK_SETUP_TOKEN_PRESENT:-1}" '{
      worker_token_signing_key:"arn:worker-token-signing-key",
      auth_key:"arn:auth-key",
      encryption_key:"arn:encryption-key",
      workspace_fencing_key:"arn:workspace-fencing-key",
      token_credential_key:"arn:token-credential-key",
      checkpoint_encryption_key:"arn:checkpoint-encryption-key"
    } + if $setup == "1" then {setup_token:"arn:setup-token",computer_wrapping_key:"arn:computer-wrapping-key"} else {} end'
    ;;
  'output -raw worker_enrollment_secret_arn') printf 'arn:worker-enrollment-token\n' ;;
  *) exit 90 ;;
esac
EOF

cat >"${tmp}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'secretsmanager get-secret-value')
    case "${MOCK_SECRET_STATUS:?}" in
      present) printf 'value\n' ;;
      missing) printf 'ResourceNotFoundException: no AWSCURRENT value\n' >&2; exit 254 ;;
      denied) printf 'AccessDeniedException: denied\n' >&2; exit 254 ;;
      *) exit 91 ;;
    esac
    ;;
  'secretsmanager put-secret-value')
    printf '%s\n' "$*" >>"${MOCK_PUT_LOG:?}"
    ;;
  *) exit 92 ;;
esac
EOF
chmod +x "${tmp}/bin/mock-tofu" "${tmp}/bin/aws"

run_helper() {
  MOCK_SECRET_STATUS="$1" \
    MOCK_PUT_LOG="${tmp}/puts" \
    TOFU="${tmp}/bin/mock-tofu" \
    PATH="${tmp}/bin:${PATH}" \
    "${script}" >"${tmp}/stdout" 2>"${tmp}/stderr"
}

: >"${tmp}/puts"
run_helper present
[ ! -s "${tmp}/puts" ] || {
  printf 'existing secret values were replaced\n' >&2
  exit 1
}

: >"${tmp}/puts"
run_helper missing
[ "$(wc -l <"${tmp}/puts" | tr -d ' ')" = 9 ] || {
  printf 'expected all nine missing secret values to be initialized\n' >&2
  exit 1
}

python3 - "${tmp}/puts" <<'PYTEST'
import base64, pathlib, shlex, sys
lines=pathlib.Path(sys.argv[1]).read_text().splitlines()
values={}
for line in lines:
    args=shlex.split(line)
    values[args[args.index('--secret-id')+1]]=args[args.index('--secret-string')+1]
assert len(base64.b64decode(values['arn:computer-wrapping-key'],validate=True))==32
assert values['arn:computer-wrapping-key']!=values['arn:encryption-key']
PYTEST

: >"${tmp}/puts"
MOCK_SETUP_TOKEN_PRESENT=0 run_helper missing
[ "$(wc -l <"${tmp}/puts" | tr -d ' ')" = 7 ] || {
  printf 'managed secret initialization required a setup token\n' >&2
  exit 1
}

: >"${tmp}/puts"
if run_helper denied; then
  printf 'secret read authorization failure was treated as a missing value\n' >&2
  exit 1
fi
[ ! -s "${tmp}/puts" ] || {
  printf 'a secret was written after authorization failure\n' >&2
  exit 1
}

printf 'ok - AWS secret initialization is create-only\n'
