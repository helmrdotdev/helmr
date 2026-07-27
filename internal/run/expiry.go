package run

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type ChildExpiryRequest struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	ParentRunID   uuid.UUID
	ChildRunID    uuid.UUID
}

// ExpireParentOwnedChild terminalizes a different-Workspace child
// that exhausted its initial queued TTL and resolves the current parent Wait
// when one is active. A parent between Attempts has no active Wait; its next
// idempotent call observes the recorded terminal child result.
func ExpireParentOwnedChild(
	ctx context.Context,
	tx pgx.Tx,
	request ChildExpiryRequest,
) (bool, error) {
	if tx == nil || request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil || request.ParentRunID == uuid.Nil ||
		request.ChildRunID == uuid.Nil {
		return false, errors.New("parent-owned child expiry authority is required")
	}
	scope := CancellationRequest{
		OrgID: request.OrgID, ProjectID: request.ProjectID,
		EnvironmentID: request.EnvironmentID,
	}
	lineage, err := cancellationLineage(ctx, tx, request.ChildRunID)
	if err != nil {
		return false, err
	}
	if len(lineage) < 2 || lineage[len(lineage)-2] != request.ParentRunID {
		return false, cancellationAuthority("queued child expiry lineage does not match", nil)
	}
	if len(lineage) > maxCancellationGraphSize {
		return false, cancellationAuthority("queued child expiry lineage exceeds the transaction bound", nil)
	}
	if err := lockCancellationActors(ctx, tx, scope, lineage); err != nil {
		return false, err
	}
	locked := make(map[uuid.UUID]cancellationRun, len(lineage))
	for _, id := range lineage {
		run, err := lockCancellationRun(ctx, tx, scope, id)
		if err != nil {
			return false, cancellationAuthority("lock queued child expiry lineage", err)
		}
		locked[id] = run
	}
	parent, parentOK := locked[request.ParentRunID]
	child, childOK := locked[request.ChildRunID]
	if !parentOK || !childOK || !child.parentRunID.Valid ||
		uuid.UUID(child.parentRunID.Bytes) != parent.id ||
		!child.parentOwnsLifecycle.Valid || !child.parentOwnsLifecycle.Bool ||
		child.workspaceID == parent.workspaceID {
		return false, cancellationAuthority("queued child expiry boundary does not match", nil)
	}
	if child.status != db.RunStatusQueued || child.currentRunLeaseID.Valid {
		return false, nil
	}
	var firstLeaseAt, queuedExpiresAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `
SELECT first_lease_at, queued_expires_at
  FROM runs
 WHERE id = $1
 FOR UPDATE`,
		child.id,
	).Scan(&firstLeaseAt, &queuedExpiresAt); err != nil {
		return false, cancellationAuthority("lock queued child expiry deadline", err)
	}
	if firstLeaseAt.Valid || !queuedExpiresAt.Valid {
		return false, nil
	}
	var expired bool
	if err := tx.QueryRow(ctx,
		`SELECT queued_expires_at <= transaction_timestamp() FROM runs WHERE id = $1`,
		child.id,
	).Scan(&expired); err != nil {
		return false, cancellationAuthority("read queued child expiry deadline", err)
	}
	if !expired {
		return false, nil
	}
	waits, err := lockCancellationResources(
		ctx,
		tx,
		lineage,
		[]cancellationRun{child},
	)
	if err != nil {
		return false, err
	}
	wait, hasWait := waits[child.id]
	if !hasWait && parent.status != db.RunStatusQueued &&
		parent.status != db.RunStatusRunning &&
		parent.status != db.RunStatusWaiting &&
		parent.status != db.RunStatusRetryDelayed {
		return false, cancellationAuthority(
			"queued child expiry has no active parent Wait",
			nil,
		)
	}
	if hasWait && (wait.runID != parent.id ||
		!wait.childRunID.Valid ||
		uuid.UUID(wait.childRunID.Bytes) != child.id ||
		wait.attemptNumber != parent.currentAttemptNumber ||
		wait.expectedRunStateVersion != parent.stateVersion ||
		wait.handoffRuntimeInstanceID.Valid ||
		wait.handoffWorkspaceMountID.Valid) {
		return false, cancellationAuthority("queued child expiry Wait does not match", nil)
	}
	if err := expireLockedParentOwnedChild(ctx, tx, child); err != nil {
		return false, err
	}
	if hasWait {
		result, err := json.Marshal(map[string]any{
			"ok": false,
			"error": map[string]any{
				"code": "queued_ttl_expired", "message": "Child Run queued TTL expired",
				"retryable": false,
			},
			"run": map[string]any{"id": child.publicID},
		})
		if err != nil {
			return false, err
		}
		if err := resolveDifferentWorkspaceChildWait(
			ctx, tx, parent, wait, result,
		); err != nil {
			return false, err
		}
	}
	return true, nil
}

func expireLockedParentOwnedChild(
	ctx context.Context,
	tx pgx.Tx,
	child cancellationRun,
) error {
	errorPayload := json.RawMessage(
		`{"code":"queued_ttl_expired","message":"Child Run queued TTL expired","retryable":false}`,
	)
	if _, err := tx.Exec(ctx, `
WITH closing_runtimes AS (
    UPDATE runtime_instances
       SET desired_state = 'closed',
           desired_version = CASE
               WHEN desired_state = 'closed' THEN desired_version
               ELSE desired_version + 1
           END,
           desired_at = transaction_timestamp(),
           desired_reason = 'queued_ttl_expired',
           updated_at = transaction_timestamp()
     WHERE reserved_run_id = $1
       AND observed_state IN ('allocated', 'preparing', 'ready', 'closing')
    RETURNING id
)
UPDATE workspace_mounts
   SET state = 'unmounting',
       stopped_at = COALESCE(stopped_at, transaction_timestamp()),
       updated_at = transaction_timestamp()
 WHERE runtime_instance_id IN (SELECT id FROM closing_runtimes)
   AND state IN ('mounting', 'mounted')`,
		child.id,
	); err != nil {
		return cancellationAuthority("request expired queued child runtime cleanup", err)
	}
	command, err := tx.Exec(ctx, `
UPDATE run_attempts
   SET terminal_outcome = 'cancelled',
       terminal_reason_code = 'queued_ttl_expired',
       terminal_error = $3::jsonb,
       terminal_at = transaction_timestamp()
 WHERE run_id = $1
   AND number = $2
   AND terminal_at IS NULL`,
		child.id,
		child.currentAttemptNumber,
		errorPayload,
	)
	if err != nil || command.RowsAffected() != 1 {
		return cancellationAuthority("expire queued child Attempt", err)
	}
	command, err = tx.Exec(ctx, `
UPDATE runs
   SET status = 'expired',
       terminal_reason_code = 'queued_ttl_expired',
       error = $2::jsonb,
       state_version = state_version + 1,
       retry_at = NULL,
       terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND state_version = $3
   AND status = 'queued'
   AND current_run_lease_id IS NULL
   AND first_lease_at IS NULL
   AND queued_expires_at IS NOT NULL
   AND queued_expires_at <= transaction_timestamp()`,
		child.id,
		errorPayload,
		child.stateVersion,
	)
	if err != nil || command.RowsAffected() != 1 {
		return cancellationAuthority("expire queued child Run", err)
	}
	command, err = tx.Exec(ctx, `
UPDATE workspaces
   SET owner_run_id = NULL,
       ownership_generation = ownership_generation + 1,
       state_version = state_version + 1,
       last_activity_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND owner_run_id = $2
   AND owner_actor_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )`,
		child.workspaceID,
		child.id,
	)
	if err != nil || command.RowsAffected() != 1 {
		return cancellationAuthority("release expired queued child Workspace", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO telemetry_outbox (
    org_id, stream_kind, source_kind, source_id, project_id, environment_id,
    run_id, attempt_number, trace_id, span_id, category, severity, source,
    kind, message, payload, redaction_class, snapshot_version, observed_at
)
SELECT org_id, 'event', 'run', id, project_id, environment_id,
       id, current_attempt_number, trace_id, root_span_id, 'lifecycle', 'info',
       'control', 'run.expired', 'Run expired',
       jsonb_build_object('reasonCode', 'queued_ttl_expired'),
       'internal', state_version, transaction_timestamp()
  FROM runs
 WHERE id = $1`,
		child.id,
	); err != nil {
		return cancellationAuthority("record queued child expiry event", err)
	}
	return nil
}
