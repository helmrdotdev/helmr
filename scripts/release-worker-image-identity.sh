#!/usr/bin/env bash
set -euo pipefail

release_tag="${1:-}"
base_name="${2:-helmr-release-image}"
base_state_key="${3:-helmr/stacks/release-worker-image/terraform.tfstate}"

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{ print $1 }'
  else
    die "sha256sum or shasum is required"
  fi
}

slugify() {
  printf '%s' "$1" |
    tr '[:upper:]' '[:lower:]' |
    sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}

[ -n "$release_tag" ] || die "usage: scripts/release-worker-image-identity.sh <release-tag> [base-name] [base-state-key]"

if ! printf '%s' "$release_tag" | grep -Eq '^v[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?$'; then
  die "release tag must match vX.Y.Z or vX.Y.Z-prerelease"
fi

release_version="${release_tag#v}"
image_builder_version="${release_version%%-*}"
if ! printf '%s' "$image_builder_version" | grep -Eq '^[0-9]+[.][0-9]+[.][0-9]+$'; then
  die "failed to derive EC2 Image Builder version from ${release_tag}"
fi

release_slug_base="$(slugify "$release_tag")"
[ -n "$release_slug_base" ] || die "failed to derive release slug from ${release_tag}"
release_tag_hash="$(printf '%s' "$release_tag" | sha256_hex | cut -c 1-10)"
release_slug="${release_slug_base}-${release_tag_hash}"
[ -n "$release_slug" ] || die "failed to derive release slug from ${release_tag}"

name_prefix="$(slugify "$base_name")"
[ -n "$name_prefix" ] || die "failed to derive worker image name prefix from ${base_name}"

worker_image_name="${name_prefix}-${release_slug}"
max_worker_image_name_length=43
if [ "${#worker_image_name}" -gt "$max_worker_image_name_length" ]; then
  hash="$(printf '%s' "$worker_image_name" | sha256_hex | cut -c 1-10)"
  prefix_length=$((max_worker_image_name_length - 11))
  worker_image_name_prefix="${worker_image_name:0:$prefix_length}"
  worker_image_name_prefix="$(printf '%s' "$worker_image_name_prefix" | sed -E 's/-+$//')"
  worker_image_name="${worker_image_name_prefix}-${hash}"
fi

state_namespace="$base_state_key"
case "$state_namespace" in
  */*) state_namespace="${state_namespace%/*}" ;;
  *.tfstate) state_namespace="${state_namespace%.tfstate}" ;;
esac
state_namespace="$(printf '%s' "$state_namespace" | sed -E 's#/+$##')"
[ -n "$state_namespace" ] || state_namespace="release-worker-image"
state_key="${state_namespace}/releases/${release_slug}.tfstate"

printf 'release_slug=%s\n' "$release_slug"
printf 'worker_image_name=%s\n' "$worker_image_name"
printf 'worker_image_version=%s\n' "$image_builder_version"
printf 'state_key=%s\n' "$state_key"
