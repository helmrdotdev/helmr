#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
output="${1:-$repo_root/dist/platform-release}"
output=$(mkdir -p "$output" && cd -- "$output" && pwd -P)
release=$(nix build -L --no-link --print-out-paths "$repo_root#platformRelease")
"$repo_root/scripts/check-canonical-json.sh" "$release/platform-release.json"

rm -f \
  "$output/platform-release.tar" \
  "$output/platform-release-provenance.json" \
  "$output/platform-release.sigstore.json"
tar \
  --create \
  --file "$output/platform-release.tar" \
  --format=ustar \
  --sort=name \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  --mode='a=rX' \
  --mtime='@0' \
  --directory "$release" \
  .

archive_sha256=$(sha256sum "$output/platform-release.tar" | awk '{print $1}')
archive_size=$(stat -c '%s' "$output/platform-release.tar")
build_policy_digest=$(cat "$release/build-policy.digest")
jq -cS \
  --arg archiveDigest "sha256:$archive_sha256" \
  --arg buildPolicyDigest "$build_policy_digest" \
  --arg sourceCommit "${GITHUB_SHA:?GITHUB_SHA is required}" \
  --arg sourceRef "${GITHUB_REF:?GITHUB_REF is required}" \
  --argjson archiveSizeBytes "$archive_size" \
  '{
    archive: {
      digest: $archiveDigest,
      mediaType: "application/vnd.helmr.platform-release.v0+tar",
      sizeBytes: $archiveSizeBytes
    },
    buildPolicyDigest: $buildPolicyDigest,
    formatVersion: 0,
    sourceCommit: $sourceCommit,
    sourceRef: $sourceRef
  }' >"$output/platform-release-provenance.json"
cosign sign-blob \
  --yes \
  --new-bundle-format=true \
  --bundle "$output/platform-release.sigstore.json" \
  "$output/platform-release.tar"
chmod 0444 "$output/"*
