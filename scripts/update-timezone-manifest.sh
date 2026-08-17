#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
timezone_data="$(nix build --no-link --print-out-paths "${repo_root}#timezoneData")"
install -m 0644 \
  "${timezone_data}/tzdb_names.txt" \
  "${repo_root}/internal/schedule/tzdb_names.txt"
