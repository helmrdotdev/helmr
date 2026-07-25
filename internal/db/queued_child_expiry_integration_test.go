package db

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParentOwnedQueuedChildExpiryResolvesEveryWaitState(t *testing.T) {
	for _, suspension := range []RunWaitState{
		RunWaitStateHot,
		RunWaitStateCheckpointing,
		RunWaitStateParked,
	} {
		t.Run(string(suspension), func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunLeaseClaimFixture(t, ctx)
			parent := newTokenWaitReconcileSetup(
				t, ctx, fixture, suspension, time.Now().Add(time.Hour),
			)
			child := fixture.addWork(t, ctx, "assigned", time.Now().Add(-time.Minute))
			claimID := uuid.Must(uuid.NewV7())
			childPublicID := runCancellationPublicID(t, fixture, child.runID)

			tx, err := fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
				t.Fatal(err)
			}
			mustRunLeaseExec(t, ctx, tx, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, scope_hash, key_hash,
    hash_key_version, generation, request_fingerprint, accepted_at
) VALUES ($1, $2, 'task.child.invoke', $3, $4, 1, 1, $5, now())`,
				claimID,
				fixture.environmentID,
				runLeaseTestHash("queued-child-expiry-scope"),
				runLeaseTestHash("queued-child-expiry-key"),
				runLeaseTestHash("queued-child-expiry-request"),
			)
			mustRunLeaseExec(t, ctx, tx, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE owner_run_lease_id = $1`,
				child.leaseID,
			)
			mustRunLeaseExec(t, ctx, tx, `
UPDATE run_leases
   SET state = 'cancelled', terminal_at = now(),
       terminal_reason_code = 'test_reset'
 WHERE id = $1`,
				child.leaseID,
			)
			mustRunLeaseExec(t, ctx, tx, `
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
			mustRunLeaseExec(t, ctx, tx, `
UPDATE run_waits
   SET kind = 'child',
       token_id = NULL,
       token_registration_run_state_version = NULL,
       child_run_id = $1,
       child_parent_owned = true,
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

			tx, err = fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			changed, err := ExpireParentOwnedChildInTransaction(
				ctx,
				tx,
				ParentOwnedChildExpiryRequest{
					OrgID: fixture.orgID, ProjectID: fixture.projectID,
					EnvironmentID: fixture.environmentID,
					ParentRunID:   parent.runID, ChildRunID: child.runID,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("expired queued child was not reconciled")
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			var childStatus RunStatus
			var reason string
			var ownerRunID *uuid.UUID
			if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, runs.terminal_reason_code, workspaces.owner_run_id
  FROM runs
  JOIN workspaces ON workspaces.id = runs.workspace_id
 WHERE runs.id = $1`,
				child.runID,
			).Scan(&childStatus, &reason, &ownerRunID); err != nil {
				t.Fatal(err)
			}
			if childStatus != RunStatusExpired || reason != "queued_ttl_expired" ||
				ownerRunID != nil {
				t.Fatalf(
					"child expiry = status:%s reason:%s owner:%v",
					childStatus, reason, ownerRunID,
				)
			}
			var result json.RawMessage
			var waitState WaitState
			var suspensionState RunWaitState
			if err := fixture.pool.QueryRow(ctx, `
SELECT condition_result, condition_state, suspension_state
  FROM run_waits
 WHERE id = $1`,
				parent.waitID,
			).Scan(&result, &waitState, &suspensionState); err != nil {
				t.Fatal(err)
			}
			wantResult := `{"ok": false, "run": {"id": "` + childPublicID +
				`"}, "error": {"code": "queued_ttl_expired", "message": "Child Run queued TTL expired", "retryable": false}}`
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
			if waitState != WaitStateCompleted {
				t.Fatalf("condition state = %s", waitState)
			}
			switch suspension {
			case RunWaitStateHot:
				if suspensionState != RunWaitStateReleased {
					t.Fatalf("hot suspension = %s", suspensionState)
				}
			case RunWaitStateCheckpointing:
				if suspensionState != RunWaitStateCheckpointing {
					t.Fatalf("checkpointing suspension = %s", suspensionState)
				}
			case RunWaitStateParked:
				if suspensionState != RunWaitStateResumePending {
					t.Fatalf("parked suspension = %s", suspensionState)
				}
			}
		})
	}
}
