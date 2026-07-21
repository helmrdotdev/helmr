#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
output="$repo_root/internal/runtime/entry.mjs"
temporary="$(mktemp "${TMPDIR:-/tmp}/helmr-runtime-entry.XXXXXX")"
trap 'rm -f "$temporary"' EXIT

cd "$repo_root"
bun build runtime/typescript/src/program.ts \
  --target=node \
  --format=esm \
  --outfile "$temporary"

if [ "${1:-}" = "--check" ]; then
  if ! cmp -s "$temporary" "$output"; then
    printf '%s\n' "internal/runtime/entry.mjs is stale" >&2
    exit 1
  fi
  exit 0
fi

install -D -m0644 "$temporary" "$output"
