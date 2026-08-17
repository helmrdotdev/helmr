package run

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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
	expiry, err := db.New(tx).LockQueuedRunExpiry(ctx, pgvalue.UUID(child.id))
	if err != nil {
		return false, cancellationAuthority("lock queued child expiry deadline", err)
	}
	if expiry.FirstLeaseAt.Valid || !expiry.QueuedExpiresAt.Valid {
		return false, nil
	}
	if !expiry.Expired {
		return false, nil
	}
	waits, err := lockCancellationResources(
		ctx,
		tx,
		lineage,
		[]cancellationRun{child},
		nil,
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
			"queued child expiry has no active parent wait",
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
		return false, cancellationAuthority("queued child expiry wait does not match", nil)
	}
	if err := expireLockedParentOwnedChild(ctx, tx, child); err != nil {
		return false, err
	}
	if hasWait {
		result, err := marshalChildFailureResult(
			child.id,
			"queued_ttl_expired",
			"Child Run queued TTL expired",
		)
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
	failure, err := MarshalFailure(
		"queued_ttl_expired",
		"Child Run queued TTL expired",
		nil,
	)
	if err != nil {
		return err
	}
	errorPayload := json.RawMessage(
		`{"code":"queued_ttl_expired","message":"Child Run queued TTL expired","retryable":false}`,
	)
	q := db.New(tx)
	childID := pgvalue.UUID(child.id)
	if err := q.RequestQueuedRunRuntimeCleanup(ctx, childID); err != nil {
		return cancellationAuthority("request expired queued child runtime cleanup", err)
	}
	rows, err := q.ExpireQueuedRunAttempt(ctx, db.ExpireQueuedRunAttemptParams{
		ErrorPayload:  errorPayload,
		RunID:         childID,
		AttemptNumber: child.currentAttemptNumber,
	})
	if err != nil || rows != 1 {
		return cancellationAuthority("expire queued child attempt", err)
	}
	rows, err = q.ExpireQueuedRun(ctx, db.ExpireQueuedRunParams{
		Failure:              failure,
		ID:                   childID,
		ExpectedStateVersion: child.stateVersion,
	})
	if err != nil || rows != 1 {
		return cancellationAuthority("expire queued child run", err)
	}
	rows, err = q.ReleaseQueuedRunWorkspace(ctx, db.ReleaseQueuedRunWorkspaceParams{
		WorkspaceID: pgvalue.UUID(child.workspaceID),
		RunID:       childID,
	})
	if err != nil || rows != 1 {
		return cancellationAuthority("release expired queued child workspace", err)
	}
	if err := q.CreateQueuedRunExpiryEvent(ctx, childID); err != nil {
		return cancellationAuthority("record queued child expiry event", err)
	}
	return nil
}
