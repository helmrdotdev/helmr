package db

import "testing"

func TestOwnershipCheckpointFKBindings(t *testing.T) {
	f := newRunLeaseClaimFixture(t, t.Context())
	var count int
	if err := f.pool.QueryRow(t.Context(), `
 SELECT count(*) FROM pg_constraint
  WHERE confrelid = 'run_checkpoints'::regclass
    AND (
        (conname IN ('run_waits_suspend_checkpoint_fk', 'runtime_instances_restore_checkpoint_execution_fkey')
         AND conindid = 'run_checkpoints_run_id_attempt_number_workspace_id_id_key'::regclass)
        OR
        (conname = 'runtime_instances_restore_checkpoint_workspace_fkey'
         AND conindid = 'run_checkpoints_id_workspace_id_key'::regclass)
    )
 `).Scan(&count); err != nil || count != 3 {
		t.Fatalf("checkpoint FK bindings = %d, error = %v", count, err)
	}
}
