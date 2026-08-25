#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/publish-platform-release.sh"

if rg -F 'docker run' "${script}" >/dev/null ||
  rg -F 'release publish' "${script}" >/dev/null; then
  printf 'not ok - signed publication must delegate after verification instead of owning a second publisher\n' >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
repo="${tmp}/repo"
mkdir -p "${repo}/scripts" "${tmp}/bin" "${tmp}/release"
cp "${script}" "${repo}/scripts/publish-platform-release.sh"
cat >"${repo}/scripts/publish-materialized-platform-release.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'publish\n' >>"${EVENTS}"
[ "$1" = s3://platform.example/releases ]
[ -f "$2/platform-release.json" ]
EOF
chmod +x "${repo}/scripts/"*.sh
printf 'tracked\n' >"${repo}/tracked"
git init -q "${repo}"
git -C "${repo}" config user.email test@example.invalid
git -C "${repo}" config user.name test
git -C "${repo}" add scripts tracked
git -C "${repo}" -c commit.gpgsign=false commit -qm initial
git -C "${repo}" tag v0.0.1
source_commit="$(git -C "${repo}" rev-parse HEAD)"

cat >"${tmp}/bin/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'cosign\n' >>"${EVENTS}"
[ "${COSIGN_FAIL:-0}" != 1 ]
EOF
chmod +x "${tmp}/bin/cosign"
printf '{}\n' >"${tmp}/release/platform-release.json"
tar -C "${tmp}/release" -cf "${tmp}/release.tar" platform-release.json
printf 'bundle\n' >"${tmp}/sigstore.bundle"
if command -v sha256sum >/dev/null 2>&1; then
  archive_digest="sha256:$(sha256sum "${tmp}/release.tar" | awk '{print $1}')"
else
  archive_digest="sha256:$(shasum -a 256 "${tmp}/release.tar" | awk '{print $1}')"
fi
archive_size="$(wc -c <"${tmp}/release.tar" | tr -d ' ')"
write_provenance() {
  jq -cn \
    --arg digest "$1" \
    --argjson size "${archive_size}" \
    --arg sourceCommit "${source_commit}" \
    '{formatVersion:0,archive:{digest:$digest,sizeBytes:$size},sourceCommit:$sourceCommit,sourceRef:"refs/tags/v0.0.1"}' \
    >"${tmp}/provenance.json"
}
write_provenance "${archive_digest}"

events="${tmp}/events"
printf 'new head\n' >"${repo}/tracked"
git -C "${repo}" add tracked
git -C "${repo}" -c commit.gpgsign=false commit -qm newer
: >"${events}"
if EVENTS="${events}" PATH="${tmp}/bin:${PATH}" \
  "${repo}/scripts/publish-platform-release.sh" s3://platform.example/releases v0.0.1 \
  "${tmp}/release.tar" "${tmp}/provenance.json" "${tmp}/sigstore.bundle" \
  >"${tmp}/tag.out" 2>"${tmp}/tag.err"; then
  printf 'not ok - checkout/tag mismatch must stop publication\n' >&2
  exit 1
fi
[ ! -s "${events}" ]
git -C "${repo}" reset -q --hard v0.0.1

if EVENTS="${events}" COSIGN_FAIL=1 PATH="${tmp}/bin:${PATH}" \
  "${repo}/scripts/publish-platform-release.sh" s3://platform.example/releases v0.0.1 \
  "${tmp}/release.tar" "${tmp}/provenance.json" "${tmp}/sigstore.bundle" \
  >"${tmp}/cosign.out" 2>"${tmp}/cosign.err"; then
  printf 'not ok - failed signature verification must stop publication\n' >&2
  exit 1
fi
[ "$(cat "${events}")" = cosign ]

: >"${events}"
write_provenance 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
if EVENTS="${events}" PATH="${tmp}/bin:${PATH}" \
  "${repo}/scripts/publish-platform-release.sh" s3://platform.example/releases v0.0.1 \
  "${tmp}/release.tar" "${tmp}/provenance.json" "${tmp}/sigstore.bundle" \
  >"${tmp}/provenance.out" 2>"${tmp}/provenance.err"; then
  printf 'not ok - mismatched provenance must stop publication\n' >&2
  exit 1
fi
[ "$(cat "${events}")" = cosign ]

: >"${events}"
write_provenance "${archive_digest}"
EVENTS="${events}" PATH="${tmp}/bin:${PATH}" \
  "${repo}/scripts/publish-platform-release.sh" s3://platform.example/releases v0.0.1 \
  "${tmp}/release.tar" "${tmp}/provenance.json" "${tmp}/sigstore.bundle"
[ "$(sed -n '1p' "${events}")" = cosign ]
[ "$(sed -n '2p' "${events}")" = publish ]
[ "$(wc -l <"${events}" | tr -d ' ')" = 2 ]

printf 'ok - platform release publisher contract\n'
