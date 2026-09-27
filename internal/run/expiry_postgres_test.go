package run

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestParentOwnedQueuedChildExpiryResolvesEveryWaitStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		suspension db.RunWaitStatus
		actor      bool
	}{
		{"hot", db.RunWaitStatusHot, false},
		{"checkpointing", db.RunWaitStatusCheckpointing, false},
		{"parked", db.RunWaitStatusParked, false},
		{"active Actor hot", db.RunWaitStatusHot, true},
		{"active Actor parked", db.RunWaitStatusParked, true},
	} {
		suspension := test.suspension
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newPostgresFixture(t)
			parent := newQueuedChildParent(t, ctx, fixture, suspension)
			child := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
			claimID := uuid.NewV7()
			childID := child.runID.String()

			tx, err := fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at
) VALUES ($1, $2, 'task.child.invoke', $3, $4, now())`,
				claimID,
				fixture.environmentID,
				dbtest.Hash("queued-child-expiry-slot"),
				dbtest.Hash("queued-child-expiry-request"),
			)
			dbtest.MustExec(t, ctx, tx, `
UPDATE run_leases
   SET status = 'cancelled', terminal_at = now(), process_reconciled_at=now(),
       terminal_reason_code = 'test_reset'
 WHERE id = $1`,
				child.leaseID,
			)
			dbtest.MustExec(t, ctx, tx, `
UPDATE runs
   SET cause_kind = 'child',
       parent_run_id = $1,
       parent_owns_lifecycle = true,
       claim_id = $2,
       current_run_lease_id = NULL,
       first_lease_at = NULL,
       queued_expires_at = now() - interval '1 second'
 WHERE id = $3`,
				parent.runID,
				claimID,
				child.runID,
			)
			dbtest.MustExec(t, ctx, tx, `
UPDATE run_waits
   SET kind = 'child',
       token_id = NULL,
       token_registration_run_revision = NULL,
       due_at = NULL,
       child_run_id = $1,
       child_target_declared_id = 'test-task',
       child_claim_id = $2,
       child_request = '{}'::jsonb
 WHERE id = $3`,
				child.runID,
				claimID,
				parent.waitID,
			)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			var actorID, inputID uuid.UUID
			if test.actor {
				actorID = fixture.convertToActor(t, ctx, leasedRun{runID: parent.runID, leaseID: parent.leaseID}, `{"enabled":false}`)
				inputID = uuid.NewV7()
				dbtest.MustExec(t, ctx, fixture.pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data) VALUES($1,$2,$3,2,'{}')`, inputID, fixture.environmentID, actorID)
				admission, err := fixture.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer admission.Rollback(context.Background())
				if _, err := db.New(admission).ActivateSessionTurn(ctx, db.ActivateSessionTurnParams{EnvironmentID: pgvalue.UUID(fixture.environmentID), SessionID: pgvalue.UUID(actorID), TurnID: pgvalue.UUID(inputID), RunID: pgvalue.UUID(parent.runID), AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, InputSequence: 2}); err != nil {
					t.Fatal(err)
				}
				if err := admission.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				// This owner is also used by child execution failure/recovery.
				graphTx, err := fixture.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer graphTx.Rollback(context.Background())
				if _, err := LockOwnedFinalization(ctx, graphTx, OwnedFinalizationRequest{OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID, RunID: child.runID}); err != nil {
					t.Fatal(err)
				}
				if err := graphTx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}

			worker, err := NewQueuedChildExpiryWorker(nil, fixture.pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.expire(ctx, 1); err != nil {
				t.Fatal(err)
			}

			var childStatus db.RunStatus
			var failureCode string
			if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, runs.failure->>'code'
  FROM runs
  JOIN computers ON computers.id = runs.computer_id
 WHERE runs.id = $1`,
				child.runID,
			).Scan(&childStatus, &failureCode); err != nil {
				t.Fatal(err)
			}
			if childStatus != db.RunStatusExpired || failureCode != "queued_ttl_expired" {
				t.Fatalf(
					"child expiry = status:%s failure:%s",
					childStatus, failureCode,
				)
			}
			var result json.RawMessage
			var waitStatus db.WaitStatus
			var suspensionStatus db.RunWaitStatus
			if err := fixture.pool.QueryRow(ctx, `
SELECT condition_result, condition_status, suspension_status
  FROM run_waits
 WHERE id = $1`,
				parent.waitID,
			).Scan(&result, &waitStatus, &suspensionStatus); err != nil {
				t.Fatal(err)
			}
			wantResult := `{"ok": false, "run": {"id": "` + childID +
				`"}, "failure": {"code": "queued_ttl_expired", "message": "Child Run queued TTL expired", "details": {}}}`
			var gotValue, wantValue any
			if err := json.Unmarshal(result, &gotValue); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(wantResult), &wantValue); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Fatalf("condition result = %s", result)
			}
			if waitStatus != db.WaitStatusCompleted {
				t.Fatalf("condition state = %s", waitStatus)
			}
			if test.actor {
				var active uuid.UUID
				var cursor int64
				var status, turnStatus string
				if err := fixture.pool.QueryRow(ctx, `SELECT s.active_turn_id,s.committed_input_sequence,s.status,r.status FROM sessions s JOIN session_turns r ON r.id=s.active_turn_id WHERE s.id=$1`, actorID).Scan(&active, &cursor, &status, &turnStatus); err != nil {
					t.Fatal(err)
				}
				if active != inputID || cursor != 1 || status != "open" || turnStatus != "running" {
					t.Fatalf("child expiry changed parent Turn: %s %d %s %s", active, cursor, status, turnStatus)
				}
			}

			switch suspension {
			case db.RunWaitStatusHot:
				if suspensionStatus != db.RunWaitStatusReleased {
					t.Fatalf("hot suspension = %s", suspensionStatus)
				}
			case db.RunWaitStatusCheckpointing:
				if suspensionStatus != db.RunWaitStatusCheckpointing {
					t.Fatalf("checkpointing suspension = %s", suspensionStatus)
				}
			case db.RunWaitStatusParked:
				if suspensionStatus != db.RunWaitStatusResumePending {
					t.Fatalf("parked suspension = %s", suspensionStatus)
				}
			}
		})
	}
}

type queuedChildParent struct {
	waitID       uuid.UUID
	runID        uuid.UUID
	computerID   uuid.UUID
	leaseID      uuid.UUID
	checkpointID pgtype.UUID
}

func newQueuedChildParent(
	t *testing.T,
	ctx context.Context,
	fixture postgresFixture,
	suspension db.RunWaitStatus,
) queuedChildParent {
	t.Helper()
	work := fixture.addRun(t, "starting", time.Now().Add(-time.Minute))
	parent := queuedChildParent{
		waitID:  uuid.NewV7(),
		runID:   work.runID,
		leaseID: work.leaseID,
	}
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT computer_id FROM runs WHERE id = $1`,
		work.runID,
	).Scan(&parent.computerID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE run_leases
		   SET status = 'running',
		       started_at = claimed_at
		 WHERE id = $1
	`, parent.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		UPDATE runs
		   SET status = 'waiting',
		       revision = 2,
		       started_at = transaction_timestamp(),
		       active_started_at = transaction_timestamp()
		 WHERE id = $1
	`, parent.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
		INSERT INTO run_waits (
			id, environment_id, run_id, computer_id, kind, due_at,
			expected_run_revision, attempt_number, current_run_lease_id
		) VALUES ($1, $2, $3, $4, 'timer', now() + interval '1 hour', 2, 1, $5)
	`, parent.waitID, fixture.environmentID, parent.runID, parent.computerID,
		parent.leaseID)

	switch suspension {
	case db.RunWaitStatusHot:
	case db.RunWaitStatusCheckpointing:
		dbtest.MustExec(t, ctx, fixture.pool, `
			UPDATE run_waits
			   SET suspension_status = 'checkpointing'
			 WHERE id = $1
		`, parent.waitID)
	case db.RunWaitStatusParked:
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
		checkpointID, privateID := uuid.NewV7(), uuid.NewV7()
		parent.checkpointID = pgvalue.UUID(checkpointID)
		dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoints(id,computer_id,environment_id,computer_spec_id,source_computer_instance_id,writer_generation,membership_revision,program_deployment_id,base_computer_disk_version_id)
        SELECT $2,i.computer_id,i.environment_id,i.computer_spec_id,i.id,i.writer_generation,i.membership_revision,i.program_deployment_id,r.base_computer_disk_version_id FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id JOIN runs r ON r.id=l.run_id WHERE l.id=$1`, parent.leaseID, checkpointID)
		dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,source_computer_instance_id,writer_generation)
        SELECT $2,environment_id,computer_id,base_computer_disk_version_id,$3,4096,'private',source_computer_instance_id,writer_generation FROM computer_checkpoints WHERE id=$1`, checkpointID, privateID, dbtest.Digest("parked-root"))
		artifacts := dbtest.InsertCheckpointArtifacts(t, ctx, tx, parent.runID, checkpointID.String())
		dbtest.MustExec(t, ctx, tx, `UPDATE computer_checkpoints SET status='ready',ready_at=now(),private_computer_disk_version_id=$2,manifest='{"version":1}',ready_request_fingerprint=$3,vm_config_artifact_id=$4,vm_state_artifact_id=$5,memory_artifact_id=$6,scratch_disk_artifact_id=$7 WHERE id=$1`, checkpointID, privateID, dbtest.Digest("ready-parked"), artifacts.RuntimeConfig, artifacts.VMState, artifacts.Memory, artifacts.ScratchDisk)
		dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_checkpoint_runs(checkpoint_id,environment_id,computer_id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_computer_instance_id,writer_generation)
        SELECT $2,environment_id,computer_id,run_id,attempt_number,$3,id,computer_instance_id,writer_generation FROM run_leases WHERE id=$1`, parent.leaseID, checkpointID, parent.waitID)
		dbtest.MustExec(t, ctx, tx, `UPDATE run_leases SET status='checkpointed',checkpointed_at=now(),terminal_at=now(),terminal_reason_code='checkpointed',process_reconciled_at=now() WHERE id=$1`, parent.leaseID)
		dbtest.MustExec(t, ctx, tx, `UPDATE runs SET current_run_lease_id=NULL,active_started_at=NULL WHERE id=$1`, parent.runID)
		dbtest.MustExec(t, ctx, tx, `UPDATE run_waits SET suspension_status='parked',current_run_lease_id=NULL,prior_run_lease_id=$2,suspend_checkpoint_id=$3 WHERE id=$1`, parent.waitID, parent.leaseID, checkpointID)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

	default:
		t.Fatalf("unsupported suspension %s", suspension)
	}
	return parent
}
