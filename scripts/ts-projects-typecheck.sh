#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

tsconfigs=()
while IFS= read -r tsconfig; do
  # Standalone integration examples own local SDK preparation and external
  # dependencies in their own projects; they are not root workspace projects.
  case "$tsconfig" in
    "$ROOT_DIR/examples/issue-fixer/"*|"$ROOT_DIR/examples/opencode-assistant/"*) continue ;;
  esac
  tsconfigs+=("$tsconfig")
done < <(find "$ROOT_DIR/examples" -mindepth 2 -maxdepth 4 -name tsconfig.json | sort)

for tsconfig in "${tsconfigs[@]}"; do
  project_dir="$(dirname "$tsconfig")"
  echo "typecheck ${project_dir#"$ROOT_DIR/"}"
  bunx tsc -p "$tsconfig" --noEmit
done
