#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
entry_output="$repo_root/internal/runtime/entry.mjs"
temporary="$(mktemp -d "${TMPDIR:-/tmp}/helmr-runtime.XXXXXX")"
trap 'rm -rf "$temporary"' EXIT

cd "$repo_root"
bun build runtime/typescript/src/program.ts \
  --target=node \
  --format=esm \
  --outfile "$temporary/entry.mjs"
if [ "${1:-}" = "--check" ]; then
  if ! cmp -s "$temporary/entry.mjs" "$entry_output"; then
    printf '%s\n' "Managed Runtime harness entries are stale" >&2
    exit 1
  fi
  exit 0
fi

install -m 0644 "$temporary/entry.mjs" "$entry_output"
