package run

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

func TestExecutionClaimRejectsChangedSessionFrontier(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"current Run", `UPDATE sessions SET current_run_id=NULL WHERE id=$1`},
		{"committed cursor", `UPDATE sessions SET committed_input_sequence=2 WHERE id=$1`},
		{"attempt cursor", `UPDATE run_attempts SET session_input_start_sequence=2 WHERE run_id=(SELECT current_run_id FROM sessions WHERE id=$1)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, work, request := executionClaimFixture(t)
			sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql, sessionID)
			if _, err := claimExecutionTest(t, f, request, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("changed Session admitted: %v", err)
			}
			var untouched bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='assigned' AND claimed_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("failed claim changed lease: %v %v", untouched, err)
			}
		})
	}
}

func TestExecutionClaimUsesCurrentAttemptCursor(t *testing.T) {
	f, work, request := executionClaimFixture(t)
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET committed_input_sequence=2 WHERE id=$1`, sessionID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET session_input_start_sequence=2 WHERE run_id=$1`, work.RunID)
	claimed, err := claimExecutionTest(t, f, request, true)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Run.SessionInputStartSequence.Int64 != 1 || claimed.Attempt.SessionInputStartSequence.Int64 != 2 || claimed.Session.CommittedInputSequence != 2 {
		t.Fatalf("attempt cursor: %+v", claimed.Attempt)
	}
}

func TestExecutionClaimValidatesRecordedSecretAuthority(t *testing.T) {
	for _, mutation := range []string{"none", "revoked", "revocation generation", "missing resolution", "additional placement"} {
		t.Run(mutation, func(t *testing.T) {
			f, work, request := executionClaimFixture(t)
			id, version := uuid.NewV7(), uuid.NewV7()
			_, err := db.New(f.Pool).CreateSecret(t.Context(), db.CreateSecretParams{ID: pgvalue.UUID(id), EnvironmentID: pgvalue.UUID(f.EnvironmentID), Name: "TEST_CREDENTIAL", VersionID: pgvalue.UUID(version), Nonce: make([]byte, 12), Ciphertext: make([]byte, 16)})
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secrets(computer_id,environment_id,placement_kind,placement_target,secret_id,mode) SELECT computer_id,environment_id,'env','TEST_CREDENTIAL',$2,'raw' FROM runs WHERE id=$1`, work.RunID, id)
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO secret_resolutions(id,computer_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation) SELECT $2,computer_id,id,1,'env','TEST_CREDENTIAL',$3,$4,0 FROM runs WHERE id=$1`, work.RunID, uuid.NewV7(), id, version)
			switch mutation {
			case "revoked":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=now(),revocation_generation=1 WHERE id=$1`, id)
			case "revocation generation":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE secrets SET revocation_generation=1 WHERE id=$1`, id)
			case "missing resolution":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM secret_resolutions WHERE run_id=$1`, work.RunID)
			case "additional placement":
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secrets(computer_id,environment_id,placement_kind,placement_target,secret_id,mode) SELECT computer_id,environment_id,'env','SECOND_CREDENTIAL',secret_id,mode FROM computer_secrets WHERE secret_id=$1`, id)
			}
			claimed, err := claimExecutionTest(t, f, request, true)
			if mutation == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if len(claimed.Secrets) != 1 || claimed.Secrets[0].Version.ID != pgvalue.UUID(version) {
					t.Fatalf("wrong recorded secret delivery: %+v", claimed.Secrets)
				}
				return
			}
			if !errors.Is(err, secret.ErrDeliveryUnavailable) {
				t.Fatalf("invalid secret authority admitted: %v", err)
			}
			var untouched bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT status='assigned' AND claimed_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("rejected secret delivery changed lease: %v %v", untouched, err)
			}
		})
	}
}

func TestExecutionClaimRejectsSessionGenerationChangedDuringLockWait(t *testing.T) {
	f, work, request := executionClaimFixture(t)
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, blocker, `SELECT id FROM computers WHERE id=(SELECT computer_id FROM runs WHERE id=$1) FOR UPDATE`, work.RunID)
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int32
	if err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := ClaimExecution(ctx, tx, request); done <- e }()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("claim did not wait: %v", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE sessions SET run_generation=run_generation+1 WHERE id=$1`, sessionID)
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, pgx.ErrNoRows) {
			t.Fatalf("changed generation accepted: %v", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err = f.Pool.QueryRow(ctx, `SELECT status='assigned' AND claimed_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("rejected claim changed lease: %v %v", untouched, err)
	}
}

// Worker Run readiness (observation freshness and Run pause) gates claim and
// start. Live members keep their execution authority on a paused or stale Worker.
func TestExecutionWorkerReadinessGatesOnlyClaimAndStart(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"paused Worker", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak' WHERE id=$1`},
		{"stale Worker", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour' WHERE id=$1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _, fence := executionClaimFixture(t)
			const ready = `UPDATE worker_hosts SET run_paused_reason=NULL,observed_at=clock_timestamp() WHERE id=$1`
			inTx := func(fn func(pgx.Tx) error) error {
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback(context.Background())
				if err = fn(tx); err != nil {
					return err
				}
				return tx.Commit(t.Context())
			}
			start := func(tx pgx.Tx) error { _, err := StartExecution(t.Context(), tx, fence); return err }
			live := func(tx pgx.Tx) error { _, err := LockLiveExecution(t.Context(), tx, fence); return err }

			dbtest.MustExec(t, t.Context(), f.Pool, test.sql, f.WorkerID)
			if _, err := claimExecutionTest(t, f, fence, true); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim on unready Worker=%v", err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, ready, f.WorkerID)
			if _, err := claimExecutionTest(t, f, fence, true); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql, f.WorkerID)
			if err := inTx(start); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("start on unready Worker=%v", err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, ready, f.WorkerID)
			if err := inTx(start); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql, f.WorkerID)
			if err := inTx(live); err != nil {
				t.Fatalf("live member on unready Worker=%v", err)
			}
		})
	}
}
