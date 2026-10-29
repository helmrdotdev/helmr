#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/aws-bootstrap-helmr-secrets.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/bin"

cat >"${tmp}/bin/mock-tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'output -json secret_arns')
    jq -n --arg local_key "${MOCK_LOCAL_WRAPPING_KEY_PRESENT:-1}" --arg capacity "${MOCK_CAPACITY_TOKEN_PRESENT:-0}" '{
      worker_host_credential_signing_key:"arn:worker-host-credential-signing-key",
      auth_key:"arn:auth-key",
      encryption_key:"arn:encryption-key",
      computer_fencing_key:"arn:computer-fencing-key",
      checkpoint_encryption_key:"arn:checkpoint-encryption-key",
      github_oauth_client_secret:"arn:github-oauth-client-secret"
    } + (if $local_key == "1" then {computer_wrapping_key:"arn:computer-wrapping-key"} else {} end) + (if $capacity == "1" then {capacity_token:"arn:capacity-token"} else {} end)'
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
    [ "$#" = 4 ]
    [ "$3" = --cli-input-json ]
    [[ "$4" = file://* ]]
    request_file="${4#file://}"
    python3 - "$request_file" <<'PYMODE'
import os, stat, sys
assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == 0o600
PYMODE
    printf '%s\n' "$request_file" >>"${MOCK_REQUEST_LOG:?}"
    jq -c . "$request_file" >>"${MOCK_PUT_LOG:?}"
    [ "${MOCK_WRITE_FAIL:-0}" = 0 ]
    ;;
  *) exit 92 ;;
esac
EOF
chmod +x "${tmp}/bin/mock-tofu" "${tmp}/bin/aws"

run_helper() {
  MOCK_SECRET_STATUS="$1" \
    MOCK_PUT_LOG="${tmp}/puts" \
    MOCK_REQUEST_LOG="${tmp}/requests" \
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
[ "$(wc -l <"${tmp}/puts" | tr -d ' ')" = 7 ] || {
  printf 'expected all seven missing secret values to be initialized\n' >&2
  exit 1
}

python3 - "${tmp}/puts" <<'PYTEST'
import base64, json, pathlib, sys
lines=pathlib.Path(sys.argv[1]).read_text().splitlines()
values={}
for line in lines:
    request=json.loads(line)
    values[request['SecretId']]=request['SecretString']
assert len(base64.b64decode(values['arn:computer-wrapping-key'],validate=True))==32
assert values['arn:computer-wrapping-key']!=values['arn:encryption-key']
PYTEST

: >"${tmp}/puts"
MOCK_CAPACITY_TOKEN_PRESENT=1 run_helper missing
python3 - "${tmp}/puts" <<'PYTEST'
import base64, json, pathlib, sys
values={item['SecretId']: item['SecretString'] for item in map(json.loads,pathlib.Path(sys.argv[1]).read_text().splitlines())}
token=values['arn:capacity-token']
assert len(token)==43 and len(base64.urlsafe_b64decode(token+'='))==32
PYTEST
: >"${tmp}/puts"
MOCK_CAPACITY_TOKEN_PRESENT=1 run_helper present
[ ! -s "${tmp}/puts" ]

: >"${tmp}/puts"
MOCK_LOCAL_WRAPPING_KEY_PRESENT=0 run_helper missing
[ "$(wc -l <"${tmp}/puts" | tr -d ' ')" = 6 ] || {
  printf 'managed secret initialization required a local wrapping key\n' >&2
  exit 1
}

: >"${tmp}/puts"
HELMR_GITHUB_OAUTH_CLIENT_SECRET=$'synthetic 秘密 % $ ` \"\r\nwith whitespace\n' run_helper missing
python3 - "${tmp}/puts" "${tmp}/stdout" "${tmp}/stderr" <<'PYTEST'
import json, pathlib, sys
values={item['SecretId']: item['SecretString'] for item in map(json.loads,pathlib.Path(sys.argv[1]).read_text().splitlines())}
expected='synthetic 秘密 % $ ` "\r\nwith whitespace\n'
assert values['arn:github-oauth-client-secret']==expected
for output in sys.argv[2:]:
    assert expected.encode() not in pathlib.Path(output).read_bytes()
PYTEST

: >"${tmp}/puts"
if run_helper denied; then
  printf 'secret read authorization failure was treated as a missing value\n' >&2
  exit 1
fi
[ ! -s "${tmp}/puts" ] || {
  printf 'a secret was written after authorization failure\n' >&2
  exit 1
}

if MOCK_WRITE_FAIL=1 run_helper missing; then
  printf 'secret write failure was ignored\n' >&2
  exit 1
fi

while IFS= read -r request_file; do
  [ ! -e "$request_file" ] || { printf 'secret request file was retained\n' >&2; exit 1; }
done <"${tmp}/requests"

printf 'ok - AWS secret initialization is create-only\n'
