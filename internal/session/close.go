package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ReconcileClose applies the repairable portion of an already-authoritative
// close direction. Callers must lock the complete Workspace Secret set before
// the Actor and pass both locked facts here.
func ReconcileClose(
	ctx context.Context,
	store db.Querier,
	actor db.Session,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) (db.Session, bool, error) {
	if actor.State != "closing" || !actor.CloseSequence.Valid {
		return actor, false, nil
	}
	if actor.CurrentRunID.Valid {
		return reconcileCurrentRunClose(ctx, store, actor)
	}
	workspace, err := store.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{
		EnvironmentID: actor.EnvironmentID,
		WorkspaceID:   actor.WorkspaceID,
		SessionID:     actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	activity, err := store.GetActorCloseWorkspaceActivity(ctx, workspace.ID)
	if err != nil {
		return db.Session{}, false, err
	}
	if actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		if !workspaceCanAdmit(workspace, activity) || !bindingsCanAdmit(actor, bindings) {
			return actor, true, nil
		}
		if _, err := CreateContinuation(
			ctx,
			store,
			actor,
			db.Workspace{ID: workspace.ID},
			bindings,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return actor, true, nil
			}
			return db.Session{}, false, fmt.Errorf("create actor close continuation: %w", err)
		}
		updated, err := store.GetActor(ctx, db.GetActorParams{
			EnvironmentID: actor.EnvironmentID,
			ID:            actor.ID,
		})
		return updated, false, err
	}
	if !workspaceCanAdmit(workspace, activity) {
		return actor, true, nil
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		if err == nil {
			err = ErrAuthority
		}
		return db.Session{}, false, fmt.Errorf("load actor close time: %w", err)
	}
	if _, err := store.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{
		CompletedAt:         now,
		ID:                  workspace.ID,
		EnvironmentID:       workspace.EnvironmentID,
		SessionID:           actor.ID,
		OwnershipGeneration: workspace.OwnershipGeneration,
		WriterGeneration:    workspace.WriterGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return actor, true, nil
		}
		return db.Session{}, false, fmt.Errorf("release actor close workspace owner: %w", err)
	}
	closed, err := store.CompleteIdleActorClose(ctx, db.CompleteIdleActorCloseParams{
		ClosedAt:      now,
		EnvironmentID: actor.EnvironmentID,
		SessionID:     actor.ID,
		WorkspaceID:   actor.WorkspaceID,
	})
	if err != nil {
		return db.Session{}, false, fmt.Errorf("complete idle actor close: %w", err)
	}
	return closed, false, nil
}

func reconcileCurrentRunClose(
	ctx context.Context,
	store db.Querier,
	actor db.Session,
) (db.Session, bool, error) {
	run, err := store.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{
		EnvironmentID: actor.EnvironmentID,
		RunID:         actor.CurrentRunID,
		SessionID:     actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	workspace, err := store.LockActorCloseWorkspace(ctx, db.LockActorCloseWorkspaceParams{
		EnvironmentID: actor.EnvironmentID,
		WorkspaceID:   actor.WorkspaceID,
		SessionID:     actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	attempt, err := store.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:       run.ID,
		Number:      run.CurrentAttemptNumber,
		WorkspaceID: workspace.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	if attempt.TerminalAt.Valid || actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		return actor, false, nil
	}
	wait, err := store.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
		EnvironmentID:      actor.EnvironmentID,
		RunID:              run.ID,
		AttemptNumber:      run.CurrentAttemptNumber,
		SessionID:          actor.ID,
		AfterInputSequence: actor.CloseSequence,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, false, nil
	}
	if err != nil {
		return db.Session{}, false, err
	}
	if _, err := FailWait(ctx, store, wait, "session_closed"); err != nil {
		return db.Session{}, false, fmt.Errorf("complete actor close input wait: %w", err)
	}
	return actor, false, nil
}

func workspaceCanAdmit(
	workspace db.Workspace,
	activity db.GetActorCloseWorkspaceActivityRow,
) bool {
	return workspace.State == db.WorkspaceStateActive &&
		workspace.DesiredState == db.WorkspaceDesiredStateActive &&
		workspace.DirtyState == db.WorkspaceDirtyStateClean &&
		workspace.HeadVersionID.Valid &&
		!activity.HasActiveLease &&
		!activity.HasActiveProcess &&
		!activity.HasActiveHandoff
}

func bindingsCanAdmit(
	actor db.Session,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) bool {
	for _, binding := range bindings {
		if binding.WorkspaceID != actor.WorkspaceID ||
			binding.EnvironmentID != actor.EnvironmentID ||
			binding.SecretState != "active" ||
			!binding.CurrentVersionID.Valid {
			return false
		}
	}
	return true
}
