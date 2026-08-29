#!/usr/bin/env bash
set -euo pipefail

if [ "$#" != 3 ]; then
  printf 'usage: scripts/verify-aws-release-manifest.sh RELEASE_TAG MANIFEST SIGSTORE_BUNDLE\n' >&2
  exit 2
fi

release_tag=$1
manifest=$2
sigstore_bundle=$3

if ! printf '%s' "$release_tag" | grep -Eq '^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)([.]((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?$'; then
  printf 'release tag must match vX.Y.Z or vX.Y.Z-prerelease\n' >&2
  exit 1
fi
[ -f "$manifest" ] || { printf 'AWS release manifest is missing: %s\n' "$manifest" >&2; exit 1; }
[ -f "$sigstore_bundle" ] || { printf 'AWS release manifest Sigstore bundle is missing: %s\n' "$sigstore_bundle" >&2; exit 1; }
command -v cosign >/dev/null 2>&1 || { printf 'cosign is required to verify the AWS release manifest\n' >&2; exit 1; }

cosign verify-blob \
  --bundle "$sigstore_bundle" \
  --certificate-identity "https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/$release_tag" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$manifest"
