package actorlifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/actorinput"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrAuthority = errors.New("Actor lifecycle durable authority is inconsistent")

// ReconcileClose applies the repairable portion of an already-authoritative
// close direction. Callers must lock the complete Workspace Secret set before
// the Actor and pass both locked facts here.
func ReconcileClose(
	ctx context.Context,
	store db.Querier,
	actor db.Actor,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) (db.Actor, bool, error) {
	if actor.State != "closing" || !actor.CloseSequence.Valid {
		return actor, false, nil
	}
	if actor.CurrentRunID.Valid {
		return reconcileCurrentRunClose(ctx, store, actor)
	}
	workspace, err := store.LockActorLifecycleWorkspace(ctx, db.LockActorLifecycleWorkspaceParams{
		EnvironmentID: actor.EnvironmentID,
		WorkspaceID:   actor.WorkspaceID,
		ActorID:       actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Actor{}, false, err
	}
	activity, err := store.GetActorLifecycleWorkspaceActivity(ctx, workspace.ID)
	if err != nil {
		return db.Actor{}, false, err
	}
	if actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		if !workspaceCanAdmit(workspace, activity) || !bindingsCanAdmit(actor, bindings) {
			return actor, true, nil
		}
		if _, err := actorinput.CreateContinuation(
			ctx,
			store,
			actor,
			db.Workspace{ID: workspace.ID},
			bindings,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return actor, true, nil
			}
			return db.Actor{}, false, fmt.Errorf("create Actor close continuation: %w", err)
		}
		updated, err := store.GetActor(ctx, db.GetActorParams{
			EnvironmentID: actor.EnvironmentID,
			ID:            actor.ID,
		})
		return updated, false, err
	}
	if !workspaceCanRelease(workspace, activity) {
		return actor, true, nil
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		if err == nil {
			err = ErrAuthority
		}
		return db.Actor{}, false, fmt.Errorf("load Actor close time: %w", err)
	}
	if _, err := store.ReleaseActorWorkspaceOwner(ctx, db.ReleaseActorWorkspaceOwnerParams{
		CompletedAt:         now,
		ID:                  workspace.ID,
		OrgID:               workspace.OrgID,
		ProjectID:           workspace.ProjectID,
		EnvironmentID:       workspace.EnvironmentID,
		ActorID:             actor.ID,
		OwnershipGeneration: workspace.OwnershipGeneration,
		WriterGeneration:    workspace.WriterGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return actor, true, nil
		}
		return db.Actor{}, false, fmt.Errorf("release Actor close Workspace owner: %w", err)
	}
	closed, err := store.CompleteIdleActorClose(ctx, db.CompleteIdleActorCloseParams{
		ClosedAt:      now,
		EnvironmentID: actor.EnvironmentID,
		ActorID:       actor.ID,
		WorkspaceID:   actor.WorkspaceID,
	})
	if err != nil {
		return db.Actor{}, false, fmt.Errorf("complete idle Actor close: %w", err)
	}
	return closed, false, nil
}

func reconcileCurrentRunClose(
	ctx context.Context,
	store db.Querier,
	actor db.Actor,
) (db.Actor, bool, error) {
	run, err := store.LockActorInputCurrentRun(ctx, db.LockActorInputCurrentRunParams{
		EnvironmentID: actor.EnvironmentID,
		RunID:         actor.CurrentRunID,
		ActorID:       actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Actor{}, false, err
	}
	workspace, err := store.LockActorLifecycleWorkspace(ctx, db.LockActorLifecycleWorkspaceParams{
		EnvironmentID: actor.EnvironmentID,
		WorkspaceID:   actor.WorkspaceID,
		ActorID:       actor.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, true, nil
	}
	if err != nil {
		return db.Actor{}, false, err
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
		return db.Actor{}, false, err
	}
	if attempt.TerminalAt.Valid || actor.CommittedInputSequence < actor.CloseSequence.Int64 {
		return actor, false, nil
	}
	wait, err := store.GetPendingActorInputRunWait(ctx, db.GetPendingActorInputRunWaitParams{
		EnvironmentID:      actor.EnvironmentID,
		RunID:              run.ID,
		AttemptNumber:      run.CurrentAttemptNumber,
		ActorID:            actor.ID,
		AfterInputSequence: actor.CloseSequence,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, false, nil
	}
	if err != nil {
		return db.Actor{}, false, err
	}
	if _, err := actorinput.FailWait(ctx, store, wait, "actor_closed"); err != nil {
		return db.Actor{}, false, fmt.Errorf("complete Actor close input Wait: %w", err)
	}
	return actor, false, nil
}

func workspaceCanAdmit(
	workspace db.Workspace,
	activity db.GetActorLifecycleWorkspaceActivityRow,
) bool {
	return workspace.State == db.WorkspaceStateActive &&
		workspace.DesiredState == db.WorkspaceDesiredStateActive &&
		workspace.DirtyState == db.WorkspaceDirtyStateClean &&
		workspace.HeadVersionID.Valid &&
		!activity.HasActiveLease &&
		!activity.HasActiveProcess &&
		!activity.HasActiveHandoff
}

func workspaceCanRelease(
	workspace db.Workspace,
	activity db.GetActorLifecycleWorkspaceActivityRow,
) bool {
	return workspaceCanAdmit(workspace, activity)
}

func bindingsCanAdmit(
	actor db.Actor,
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
