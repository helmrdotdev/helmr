#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/dev/instance.sh
source "${ROOT}/scripts/dev/instance.sh"

helmr_dev_init "${ROOT}"
helmr_dev_reset_owned_storage

echo "Reset owned dev storage under ${HELMR_DEV_DIR} (postgres, clickhouse, cas)."
