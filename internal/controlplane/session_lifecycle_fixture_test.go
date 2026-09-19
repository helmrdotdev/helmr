package controlplane

import "testing"

func settleActorBootRun(
	t *testing.T,
	fixture actorStartPostgresFixture,
	started actorStartResult,
	committedInputSequence int64,
) {
	t.Helper()
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE run_attempts
		   SET entrypoint_entered_at = now(),
		       terminal_session_input_sequence = $2,
		       terminal_outcome = 'succeeded',
		       terminal_reason_code = 'completed',
		       terminal_at = now()
		 WHERE run_id = $1
		   AND number = 1
	`, started.BootRunID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE runs
		   SET status = 'succeeded',
		       terminal_at = now(),
		       updated_at = now()
		 WHERE id = $1
	`, started.BootRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE sessions
		   SET current_run_id = NULL,
		       committed_input_sequence = $2,
		       run_generation = run_generation + 1,
		       revision = revision + 1,
		       updated_at = now()
		 WHERE id = $1
	`, started.SessionID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
