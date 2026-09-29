#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=dev/local/instance.sh
source "${ROOT}/dev/local/instance.sh"

helmr_dev_init "${ROOT}"
helmr_dev_reset_owned_storage

echo "Reset owned dev storage under ${HELMR_DEV_DIR} (postgres, clickhouse)."
