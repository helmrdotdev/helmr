#!/usr/bin/env bash
set -euo pipefail

# shellcheck source=case-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/case-lib.sh"
trap validation_cleanup_tmp EXIT INT TERM

if validation_dry_run; then
  exit 0
fi

validation_db_marker "
  COPY (
    SELECT 'old-epoch-fenced'
      FROM worker_instances
     WHERE current_epoch > 1
       AND startup_inventory_epoch = current_epoch
       AND startup_inventory_evidence IS NOT NULL
       AND state = 'active'
     LIMIT 1
  ) TO STDOUT;
" old-epoch-fenced || {
  validation_write_result failed live_epoch_advance_missing
  exit 1
}

validation_write_result passed null "$(validation_empty_objects)" '{
  live_epoch_advance:true,
  old_epoch_fenced:true,
  startup_recovery_recorded:true
}'
