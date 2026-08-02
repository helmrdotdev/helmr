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
      JOIN worker_groups ON worker_groups.id = worker_instances.worker_group_id
      JOIN worker_observations
        ON worker_observations.worker_instance_id = worker_instances.id
       AND worker_observations.worker_epoch = worker_instances.current_epoch
     WHERE worker_instances.current_epoch > 1
       AND worker_instances.state = 'active'
       AND worker_instances.runtime_identity_id IS NOT NULL
       AND worker_observations.observed_at >= transaction_timestamp()
           - worker_groups.observation_ttl_seconds * interval '1 second'
     LIMIT 1
  ) TO STDOUT;
" old-epoch-fenced || {
  validation_write_result failed live_epoch_advance_missing
  exit 1
}

validation_write_result passed null "$(validation_empty_objects)" '{
  live_epoch_advance:true,
  old_epoch_fenced:true,
  current_epoch_readiness_recorded:true
}'
