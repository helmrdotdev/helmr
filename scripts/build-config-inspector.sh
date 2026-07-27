#!/usr/bin/env bash
set -euo pipefail

# Regenerate the checked-in project config inspector from the pinned toolchain.
# Release CI re-runs this script and fails if internal/project/js is stale
# or contains untracked output.

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="$repo_root/internal/project/js"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/helmr-config-inspector.XXXXXX")"

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

mkdir -p "$tmp_dir/config"

cd "$repo_root"
bun build runtime/typescript/src/inspect.ts \
  --target=node \
  --format=esm \
  --outfile "$tmp_dir/config/inspect.js"
install -m 0644 runtime/typescript/src/register.mjs "$tmp_dir/config/register.mjs"
install -m 0644 runtime/typescript/src/loader.mjs "$tmp_dir/config/loader.mjs"

inspect_hash="$(sha256_file "$tmp_dir/config/inspect.js")"
register_hash="$(sha256_file "$tmp_dir/config/register.mjs")"
loader_hash="$(sha256_file "$tmp_dir/config/loader.mjs")"

cat > "$tmp_dir/config/manifest.json" <<EOF
{
  "format_version": 0,
  "files": {
    "inspect.js": "sha256:$inspect_hash",
    "register.mjs": "sha256:$register_hash",
    "loader.mjs": "sha256:$loader_hash"
  },
  "target": "node"
}
EOF

rm -rf "$out_dir"
mkdir -p "$(dirname "$out_dir")"
mv "$tmp_dir/config" "$out_dir"

printf 'config inspector updated at %s\n' "$out_dir"
