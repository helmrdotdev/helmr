#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
config_output="$repo_root/internal/compiler/config-evaluator.mjs"
program_output="$repo_root/internal/compiler/program-compiler.mjs"
temporary="$(mktemp -d "${TMPDIR:-/tmp}/helmr-compiler.XXXXXX")"
trap 'rm -rf "$temporary"' EXIT

cd "$repo_root"
bun build compiler/typescript/src/config-evaluator.ts \
  --target=node \
  --format=esm \
  --external esbuild \
  --outfile "$temporary/config-evaluator.mjs"
bun build compiler/typescript/src/program-compiler.ts \
  --target=node \
  --format=esm \
  --external esbuild \
  --outfile "$temporary/program-compiler.mjs"

if [ "${1:-}" = "--check" ]; then
  if ! cmp -s "$temporary/config-evaluator.mjs" "$config_output" ||
    ! cmp -s "$temporary/program-compiler.mjs" "$program_output"; then
    printf '%s\n' "Platform Compiler entries are stale" >&2
    exit 1
  fi
  exit 0
fi

install -D -m0644 "$temporary/config-evaluator.mjs" "$config_output"
install -D -m0644 "$temporary/program-compiler.mjs" "$program_output"
