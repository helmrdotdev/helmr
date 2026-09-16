#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
: "${RELEASE_SOURCE_COMMIT:?selected source required}" "${RELEASE_SOURCE_REF:?selected ref required}"
[ "$(git -C "$repo_root" rev-parse HEAD)" = "$RELEASE_SOURCE_COMMIT" ] || { echo 'platform source selection differs' >&2; exit 1; }
output="${1:-$repo_root/dist/platform-release}"
output=$(mkdir -p "$output" && cd -- "$output" && pwd -P)
release=$(nix build -L --no-link --print-out-paths "$repo_root#platformRelease")
"$repo_root/scripts/check-canonical-json.sh" "$release/platform-release.json"

rm -f \
  "$output/platform-release.tar" \
  "$output/platform-release-provenance.json"
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
jq -cS -n \
  --arg archiveDigest "sha256:$archive_sha256" \
  --arg sourceCommit "$RELEASE_SOURCE_COMMIT" \
  --arg sourceRef "$RELEASE_SOURCE_REF" \
  --argjson archiveSizeBytes "$archive_size" \
  '{
    archive: {
      digest: $archiveDigest,
      mediaType: "application/vnd.helmr.platform-release.v0+tar",
      sizeBytes: $archiveSizeBytes
    },
    formatVersion: 0,
    sourceCommit: $sourceCommit,
    sourceRef: $sourceRef
  }' >"$output/platform-release-provenance.json"
# Signing belongs to the fresh trusted publisher, never the build job.
chmod 0444 "$output/"*
