#!/usr/bin/env bash
set -euo pipefail

# Regenerate checked-in adapter artifacts from the pinned repo toolchain.
# Release CI re-runs this script and fails if internal/adapter/js is stale
# or contains untracked output.

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="$repo_root/internal/adapter/js"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-embedded-adapter.XXXXXX")"

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    printf 'sha256sum or shasum is required\n' >&2
    exit 1
  fi
}

mkdir -p "$tmp_dir/adapter"

cd "$repo_root"
bun build runtime/typescript/src/main.ts \
  --target=node \
  --format=esm \
  --outfile "$tmp_dir/adapter/main.js"
install -m 0644 runtime/typescript/src/register.mjs "$tmp_dir/adapter/register.mjs"
install -m 0644 runtime/typescript/src/loader.mjs "$tmp_dir/adapter/loader.mjs"

main_hash="$(sha256_file "$tmp_dir/adapter/main.js")"
register_hash="$(sha256_file "$tmp_dir/adapter/register.mjs")"
loader_hash="$(sha256_file "$tmp_dir/adapter/loader.mjs")"
if command -v sha256sum >/dev/null 2>&1; then
  proto_hash="$(sha256sum proto/*.proto | sha256sum | awk '{ print $1 }')"
else
  proto_hash="$(shasum -a 256 proto/*.proto | shasum -a 256 | awk '{ print $1 }')"
fi

cat > "$tmp_dir/adapter/manifest.json" <<EOF
{
  "runtime_contract_version": 0,
  "adapter_files": {
    "main.js": "sha256:$main_hash",
    "register.mjs": "sha256:$register_hash",
    "loader.mjs": "sha256:$loader_hash"
  },
  "proto_schema_hash": "sha256:$proto_hash",
  "bundle_target": "node"
}
EOF

rm -rf "$out_dir"
mkdir -p "$(dirname "$out_dir")"
mv "$tmp_dir/adapter" "$out_dir"

printf 'embedded adapter updated at %s\n' "$out_dir"
