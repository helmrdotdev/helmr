#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
verifier="${root}/scripts/verify-aws-release-manifest.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "${tmp}/bin"
printf '{"schema":"helmr.aws-release.v0"}\n' >"${tmp}/aws-artifacts.json"
printf '{}\n' >"${tmp}/aws-artifacts.sigstore.json"

cat >"${tmp}/bin/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >"$COSIGN_ARGS"
[ "${COSIGN_FAIL:-0}" != 1 ]
EOF
chmod +x "${tmp}/bin/cosign"

args="${tmp}/cosign.args"
COSIGN_ARGS="$args" PATH="${tmp}/bin:${PATH}" "$verifier" \
  v1.2.3-rc.1 \
  "${tmp}/aws-artifacts.json" \
  "${tmp}/aws-artifacts.sigstore.json"

cat >"${tmp}/expected.args" <<EOF
verify-blob
--bundle
${tmp}/aws-artifacts.sigstore.json
--certificate-identity
https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/v1.2.3-rc.1
--certificate-oidc-issuer
https://token.actions.githubusercontent.com
${tmp}/aws-artifacts.json
EOF
cmp "${tmp}/expected.args" "$args"

for invalid_tag in v1.2 v01.2.3 v1.2.3-01 refs/tags/v1.2.3; do
  if COSIGN_ARGS="$args" PATH="${tmp}/bin:${PATH}" "$verifier" \
    "$invalid_tag" "${tmp}/aws-artifacts.json" "${tmp}/aws-artifacts.sigstore.json" \
    >/dev/null 2>&1; then
    printf 'not ok - verifier accepted malformed release tag: %s\n' "$invalid_tag" >&2
    exit 1
  fi
done

if "$verifier" >/dev/null 2>&1; then
  printf 'not ok - verifier accepted missing arguments\n' >&2
  exit 1
fi
if COSIGN_ARGS="$args" PATH="${tmp}/bin:${PATH}" "$verifier" \
  v1.2.3 "${tmp}/missing.json" "${tmp}/aws-artifacts.sigstore.json" \
  >/dev/null 2>&1; then
  printf 'not ok - verifier accepted a missing manifest\n' >&2
  exit 1
fi
if COSIGN_ARGS="$args" PATH="${tmp}/bin:${PATH}" "$verifier" \
  v1.2.3 "${tmp}/aws-artifacts.json" "${tmp}/missing.sigstore.json" \
  >/dev/null 2>&1; then
  printf 'not ok - verifier accepted a missing Sigstore bundle\n' >&2
  exit 1
fi
if COSIGN_ARGS="$args" COSIGN_FAIL=1 PATH="${tmp}/bin:${PATH}" "$verifier" \
  v1.2.3 "${tmp}/aws-artifacts.json" "${tmp}/aws-artifacts.sigstore.json" \
  >/dev/null 2>&1; then
  printf 'not ok - verifier accepted failed signature verification\n' >&2
  exit 1
fi

printf 'ok - AWS release manifest verifier contract\n'
