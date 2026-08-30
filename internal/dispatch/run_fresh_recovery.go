package dispatch

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// RecoverRunExecutionLeases repairs physical-authority loss for every live
// non-resume Run Lease. Checkpoint-backed resume remains in
// RecoverExpiredRunResumes; both lanes share the dispatcher reconciler and
// advisory lock.
func (d *Authority) RecoverRunExecutionLeases(ctx context.Context, limit int32) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(d.pool).ListRunExecutionLeaseRecoveryCandidates(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list Run execution lease recovery candidates: %w", err)
	}
	recovered := 0
	var recoveryErrors []error
	for _, candidate := range candidates {
		changed, err := d.recoverRunExecutionLease(ctx, candidate)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			recoveryErrors = append(recoveryErrors, err)
			continue
		}
		if changed {
			recovered++
		}
	}
	return recovered, errors.Join(recoveryErrors...)
}

func (d *Authority) recoverRunExecutionLease(
	ctx context.Context,
	candidate db.ListRunExecutionLeaseRecoveryCandidatesRow,
) (bool, error) {
	runID, err := uuidFromPG(candidate.RunID)
	if err != nil {
		return false, err
	}
	workspaceID, err := uuidFromPG(candidate.WorkspaceID)
	if err != nil {
		return false, err
	}
	runLeaseID, err := uuidFromPG(candidate.RunLeaseID)
	if err != nil {
		return false, err
	}
	orgID, err := uuidFromPG(candidate.OrgID)
	if err != nil {
		return false, err
	}
	projectID, err := uuidFromPG(candidate.ProjectID)
	if err != nil {
		return false, err
	}
	environmentID, err := uuidFromPG(candidate.EnvironmentID)
	if err != nil {
		return false, err
	}
	tx, err := d.begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin Run execution lease recovery: %w", err)
	}
	defer rollback(ctx, tx)
	q := db.New(tx)
	resolutions, secretsAvailable, err := secret.LockAttemptRetryResolutions(
		ctx,
		q,
		candidate.RunID,
		candidate.CurrentAttemptNumber,
		candidate.WorkspaceID,
	)
	if err != nil {
		return false, fmt.Errorf("lock Run execution retry Secret metadata: %w", err)
	}
	graph, err := run.LockOwnedFinalization(ctx, tx, run.OwnedFinalizationRequest{
		OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, RunID: runID,
	})
	if err != nil {
		return false, fmt.Errorf("lock Run execution lease recovery graph: %w", err)
	}
	recovered, err := graph.RecoverExecutionLeaseLoss(ctx, run.ExecutionLeaseRecoveryRequest{
		RunID: runID, WorkspaceID: workspaceID, AttemptNumber: candidate.CurrentAttemptNumber,
		RunLeaseID: runLeaseID, RetryResolutions: resolutions,
		RetrySecretsAvailable: secretsAvailable,
	})
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit fresh Run lease recovery: %w", err)
	}
	return recovered, nil
}

func uuidFromPG(value pgtype.UUID) (uuid.UUID, error) {
	if !value.Valid {
		return uuid.Nil(), errors.New("Run lease recovery UUID is required")
	}
	parsed := uuid.UUID(value.Bytes)
	if parsed == uuid.Nil() {
		return uuid.Nil(), errors.New("Run lease recovery UUID is required")
	}
	return parsed, nil
}
