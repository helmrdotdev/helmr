#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

tsconfigs=()
while IFS= read -r tsconfig; do
  # This standalone integration example owns local SDK preparation and external
  # dependencies in its own project; it is not a root workspace project.
  case "$tsconfig" in "$ROOT_DIR/examples/issue-fixer/"*) continue ;; esac
  tsconfigs+=("$tsconfig")
done < <(find "$ROOT_DIR/examples" -mindepth 2 -maxdepth 4 -name tsconfig.json | sort)

for tsconfig in "${tsconfigs[@]}"; do
  project_dir="$(dirname "$tsconfig")"
  echo "typecheck ${project_dir#"$ROOT_DIR/"}"
  bunx tsc -p "$tsconfig" --noEmit
done
