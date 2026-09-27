#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

tsconfigs=()
for root in "$ROOT_DIR/examples" "$ROOT_DIR/fixtures"; do
  if [[ ! -d "$root" ]]; then
    continue
  fi
  while IFS= read -r tsconfig; do
    # This standalone integration example owns local SDK preparation and external
    # dependencies in check-dev-samples.sh; it is not a root workspace project.
    case "$tsconfig" in "$ROOT_DIR/examples/issue-fixer/"*) continue ;; esac
    tsconfigs+=("$tsconfig")
  done < <(find "$root" -mindepth 2 -maxdepth 4 -name tsconfig.json | sort)
done

for tsconfig in "${tsconfigs[@]}"; do
  project_dir="$(dirname "$tsconfig")"
  echo "typecheck ${project_dir#"$ROOT_DIR/"}"
  bunx tsc -p "$tsconfig" --noEmit
done
