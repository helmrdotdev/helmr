package db

import "testing"

func TestOwnershipCheckpointFKBindings(t *testing.T) {
	f := newRunLeaseClaimFixture(t, t.Context())
	for _, tc := range []struct {
		table, definition string
		deferred          bool
	}{
		{"computer_instances", "FOREIGN KEY (source_checkpoint_id, computer_id) REFERENCES computer_checkpoints(id, computer_id) ON DELETE RESTRICT", false},
		{"run_waits", "FOREIGN KEY (run_id, attempt_number, computer_id, id, suspend_checkpoint_id) REFERENCES computer_checkpoint_runs(run_id, attempt_number, computer_id, run_wait_id, checkpoint_id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED", true},
		{"computer_checkpoint_runs", "FOREIGN KEY (environment_id, computer_id, checkpoint_id, source_computer_instance_id, writer_generation) REFERENCES computer_checkpoints(environment_id, computer_id, id, source_computer_instance_id, writer_generation) ON DELETE RESTRICT", false},
	} {
		t.Run(tc.table, func(t *testing.T) {
			var count int
			err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE conrelid=$1::regclass AND contype='f' AND pg_get_constraintdef(oid)=$2 AND condeferrable=$3 AND condeferred=$3`, tc.table, tc.definition, tc.deferred).Scan(&count)
			if err != nil || count != 1 {
				t.Fatalf("checkpoint scope binding count=%d err=%v", count, err)
			}
		})
	}
}
