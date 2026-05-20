#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_equal() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  [ "$actual" = "$expected" ] || fail "$label: expected '$expected', got '$actual'"
}

assert_not_equal() {
  local left="$1"
  local right="$2"
  local label="$3"
  [ "$left" != "$right" ] || fail "$label: expected distinct values, got '$left'"
}

assert_length_at_most() {
  local value="$1"
  local max_length="$2"
  local label="$3"
  [ "${#value}" -le "$max_length" ] || fail "$label: expected length <= $max_length, got ${#value} ('$value')"
}

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{ print $1 }'
  else
    fail "sha256sum or shasum is required"
  fi
}

tag_hash() {
  printf '%s' "$1" | sha256_hex | cut -c 1-10
}

identity_value() {
  local key="$1"
  local file="$2"
  awk -F= -v key="$key" '$1 == key { print substr($0, length(key) + 2) }' "$file"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

"$repo_root/scripts/release-worker-image-identity.sh" \
  v0.1.1-rc.2 \
  helmr-release-image \
  helmr/stacks/release-worker-image/terraform.tfstate >"$tmp/rc2.env"

rc2_slug="v0-1-1-rc-2-$(tag_hash v0.1.1-rc.2)"
assert_equal "$rc2_slug" "$(identity_value release_slug "$tmp/rc2.env")" "rc release slug"
assert_equal "helmr-release-image-${rc2_slug}" "$(identity_value worker_image_name "$tmp/rc2.env")" "rc worker image name"
assert_equal "0.1.1" "$(identity_value worker_image_version "$tmp/rc2.env")" "rc image builder version"
assert_equal "helmr/stacks/release-worker-image/releases/${rc2_slug}.tfstate" "$(identity_value state_key "$tmp/rc2.env")" "rc state key"
assert_length_at_most "$(identity_value worker_image_name "$tmp/rc2.env")" 43 "rc worker image name should fit IAM role prefix"

"$repo_root/scripts/release-worker-image-identity.sh" \
  v1.2.3 \
  Helmr_Release_Image \
  custom/release.tfstate >"$tmp/stable.env"

stable_slug="v1-2-3-$(tag_hash v1.2.3)"
assert_equal "$stable_slug" "$(identity_value release_slug "$tmp/stable.env")" "stable release slug"
assert_equal "helmr-release-image-${stable_slug}" "$(identity_value worker_image_name "$tmp/stable.env")" "stable worker image name"
assert_equal "1.2.3" "$(identity_value worker_image_version "$tmp/stable.env")" "stable image builder version"
assert_equal "custom/releases/${stable_slug}.tfstate" "$(identity_value state_key "$tmp/stable.env")" "stable state key"

"$repo_root/scripts/release-worker-image-identity.sh" \
  v1.2.3-rc.1 \
  helmr-release-image \
  state.tfstate >"$tmp/rc-dot.env"

"$repo_root/scripts/release-worker-image-identity.sh" \
  v1.2.3-rc-1 \
  helmr-release-image \
  state.tfstate >"$tmp/rc-dash.env"

assert_not_equal "$(identity_value worker_image_name "$tmp/rc-dot.env")" "$(identity_value worker_image_name "$tmp/rc-dash.env")" "colliding slug worker image names"
assert_not_equal "$(identity_value state_key "$tmp/rc-dot.env")" "$(identity_value state_key "$tmp/rc-dash.env")" "colliding slug state keys"

if "$repo_root/scripts/release-worker-image-identity.sh" v1.2 2>/dev/null; then
  fail "invalid release tag should fail"
fi

long_name="$("$repo_root/scripts/release-worker-image-identity.sh" \
  v1.2.3-rc.this-is-a-very-long-release-candidate-name-used-to-exercise-name-truncation \
  helmr-release-image-name-with-a-long-prefix-for-testing \
  state.tfstate | awk -F= '$1 == "worker_image_name" { print $2 }')"

assert_length_at_most "$long_name" 43 "worker image name should be truncated to fit IAM role suffix"
assert_length_at_most "${long_name}-worker-image-builder" 64 "IAM role name should fit AWS limit"

printf 'ok - release worker image identity tests\n'
