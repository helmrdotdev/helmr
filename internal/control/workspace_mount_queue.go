package control

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type queuedRunWorkspaceMountStore interface {
	GetRun(context.Context, db.GetRunParams) (db.Run, error)
	EnsureWorkspaceMountRequested(context.Context, db.EnsureWorkspaceMountRequestedParams) (db.EnsureWorkspaceMountRequestedRow, error)
	SetQueuedRunWorkspaceMount(context.Context, db.SetQueuedRunWorkspaceMountParams) error
}

type queuedRunWorkspaceMountTarget struct {
	ID            pgtype.UUID
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	WorkspaceID   pgtype.UUID
	Priority      int32
	Request       []byte
}

type queuedRunWorkspaceMountLink struct {
	OrgID            pgtype.UUID
	RunID            pgtype.UUID
	WorkspaceID      pgtype.UUID
	WorkspaceMountID pgtype.UUID
}

func ensureWorkspaceMountForQueuedRun(ctx context.Context, store interface {
	EnsureWorkspaceMountRequested(context.Context, db.EnsureWorkspaceMountRequestedParams) (db.EnsureWorkspaceMountRequestedRow, error)
}, target queuedRunWorkspaceMountTarget) (db.EnsureWorkspaceMountRequestedRow, error) {
	return store.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              target.ID,
		OrgID:           target.OrgID,
		ProjectID:       target.ProjectID,
		EnvironmentID:   target.EnvironmentID,
		WorkspaceID:     target.WorkspaceID,
		RequestPriority: target.Priority,
		Request:         target.Request,
	})
}

func linkQueuedRunWorkspaceMount(ctx context.Context, store interface {
	SetQueuedRunWorkspaceMount(context.Context, db.SetQueuedRunWorkspaceMountParams) error
}, link queuedRunWorkspaceMountLink) error {
	return store.SetQueuedRunWorkspaceMount(ctx, db.SetQueuedRunWorkspaceMountParams{
		OrgID:            link.OrgID,
		RunID:            link.RunID,
		WorkspaceID:      link.WorkspaceID,
		WorkspaceMountID: link.WorkspaceMountID,
	})
}

func ensureQueuedRunWorkspaceMount(ctx context.Context, store queuedRunWorkspaceMountStore, orgID pgtype.UUID, runID pgtype.UUID, source string, log *slog.Logger) (bool, error) {
	run, err := store.GetRun(ctx, db.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		return false, err
	}
	if run.Status != db.RunStatusQueued || run.CurrentRunLeaseID.Valid || !run.WorkspaceID.Valid {
		return false, nil
	}
	request, err := json.Marshal(map[string]string{
		"source": source,
		"run_id": pgvalue.UUIDString(run.ID),
	})
	if err != nil {
		return false, err
	}
	mount, err := ensureWorkspaceMountForQueuedRun(ctx, store, queuedRunWorkspaceMountTarget{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         run.OrgID,
		ProjectID:     run.ProjectID,
		EnvironmentID: run.EnvironmentID,
		WorkspaceID:   run.WorkspaceID,
		Priority:      run.Priority,
		Request:       request,
	})
	if err != nil {
		return false, err
	}
	if log != nil {
		log.Info("queued run workspace mount ensured",
			"source", source,
			"org_id", pgvalue.UUIDString(run.OrgID),
			"run_id", pgvalue.UUIDString(run.ID),
			"workspace_id", pgvalue.UUIDString(run.WorkspaceID),
			"workspace_mount_id", pgvalue.UUIDString(mount.ID),
			"state", mount.State,
			"priority", mount.Priority,
			"inserted", mount.Inserted,
			"decision", mount.Decision,
		)
	}
	if err := linkQueuedRunWorkspaceMount(ctx, store, queuedRunWorkspaceMountLink{
		OrgID:            run.OrgID,
		RunID:            run.ID,
		WorkspaceID:      run.WorkspaceID,
		WorkspaceMountID: mount.ID,
	}); err != nil {
		return false, err
	}
	return true, nil
}
