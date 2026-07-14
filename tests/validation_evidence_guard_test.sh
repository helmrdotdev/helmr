#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/aws-dev-smoke.sh"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/bin"

cat >"${tmp}/bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"output -raw bucket_name"*) printf 'state-bucket\n' ;;
  *"output -raw source_artifact_bucket_name"*) printf 'artifact-bucket\n' ;;
  *) exit 2 ;;
esac
EOF

cat >"${tmp}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"list-object-versions"*"--prefix helmr/validation-claims/"*)
    printf '{"Versions":[{"Key":"helmr/validation-claims/campaign/claim.json","VersionId":"v1"}],"DeleteMarkers":[]}'
    ;;
  *"list-object-versions"*) printf '{"Versions":[],"DeleteMarkers":[]}' ;;
  *"delete-objects"*) printf '{}\n' ;;
  *) exit 2 ;;
esac
EOF
chmod +x "${tmp}/bin/tofu" "${tmp}/bin/aws"

if PATH="${tmp}/bin:${PATH}" TF_BIN=tofu "${script}" bootstrap-destroy-prepare >"${tmp}/stdout" 2>"${tmp}/stderr"; then
  fail "bootstrap destruction should refuse retained validation evidence"
fi
grep -Fq 'contains retained validation claims or evidence' "${tmp}/stderr" || fail "guard failure reason"

PATH="${tmp}/bin:${PATH}" TF_BIN=tofu ALLOW_VALIDATION_EVIDENCE_DELETE=1 \
  "${script}" bootstrap-destroy-prepare >"${tmp}/stdout" 2>"${tmp}/stderr"

printf 'ok - validation evidence guard tests\n'
