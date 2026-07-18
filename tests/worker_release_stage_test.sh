#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/aws-dev-smoke.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

mkdir -p "${tmp}/bin" "${tmp}/mock"
printf 'signed runtime release package\n' >"${tmp}/release.tar"

cat >"${tmp}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

command="${1:-}:${2:-}"
shift 2
case "${command}" in
  s3api:put-object)
    body=
    while (($#)); do
      case "$1" in
        --body) body="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    if [ -e "${MOCK_S3_ROOT}/object" ]; then
      exit 1
    fi
    cp "${body}" "${MOCK_S3_ROOT}/object"
    printf '%s\n' "${MOCK_DIGEST}" >"${MOCK_S3_ROOT}/digest"
    printf '%s\n' '{"VersionId":"version-1"}'
    ;;
  s3api:head-object)
    jq -cn \
      --arg digest "$(cat "${MOCK_S3_ROOT}/digest")" \
      --argjson size "${MOCK_SIZE}" \
      '{VersionId:"version-1",ContentLength:$size,Metadata:{sha256:$digest},SSEKMSKeyId:"arn:aws:kms:us-east-1:123456789012:key/example"}'
    ;;
  s3api:get-object)
    output="${!#}"
    cp "${MOCK_S3_ROOT}/object" "${output}"
    printf '%s\n' '{"VersionId":"version-1"}'
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod +x "${tmp}/bin/aws"

if command -v sha256sum >/dev/null 2>&1; then
  digest="$(sha256sum "${tmp}/release.tar" | awk '{print $1}')"
else
  digest="$(shasum -a 256 "${tmp}/release.tar" | awk '{print $1}')"
fi
size="$(wc -c <"${tmp}/release.tar" | tr -d '[:space:]')"

run_stage() {
  PATH="${tmp}/bin:${PATH}" \
    MOCK_S3_ROOT="${tmp}/mock" \
    MOCK_DIGEST="${digest}" \
    MOCK_SIZE="${size}" \
    STATE_DIR="${tmp}/state" \
    WORKER_IMAGE_RELEASE_PACKAGE="${tmp}/release.tar" \
    WORKER_IMAGE_RELEASE_PACKAGE_BUCKET="release-bucket" \
    WORKER_IMAGE_RELEASE_PACKAGE_SHA256="${EXPECTED_DIGEST_OVERRIDE:-${digest}}" \
    WORKER_IMAGE_RELEASE_PACKAGE_SIZE_BYTES="${EXPECTED_SIZE_OVERRIDE:-${size}}" \
    "${script}" worker-release-stage
}

run_stage >/dev/null
[ "$(cat "${tmp}/state/worker-release-package-version-id")" = "version-1" ] ||
  fail "version ID was not recorded"
[ "$(cat "${tmp}/state/worker-release-package-sha256")" = "${digest}" ] ||
  fail "package digest was not recorded"
[ "$(cat "${tmp}/state/worker-release-package-kms-key-arn")" = "arn:aws:kms:us-east-1:123456789012:key/example" ] ||
  fail "KMS key ARN was not recorded"
grep -Eq '^s3://release-bucket/helmr/runtime-release-packages/[0-9a-f]{40}/[0-9a-f]{64}[.]tar$' \
  "${tmp}/state/worker-release-package-s3-uri" ||
  fail "immutable staging URI was not recorded"

run_stage >/dev/null

if EXPECTED_DIGEST_OVERRIDE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  run_stage >/dev/null 2>&1; then
  fail "staging accepted bytes that differ from the verifier digest"
fi
if EXPECTED_SIZE_OVERRIDE="$((size + 1))" run_stage >/dev/null 2>&1; then
  fail "staging accepted bytes that differ from the verifier length"
fi

printf 'different bytes\n' >"${tmp}/mock/object"
if run_stage >/dev/null 2>&1; then
  fail "staging accepted divergent bytes at an existing object version"
fi

printf 'ok - Worker runtime-release staging tests\n'
