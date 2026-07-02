package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	runWaitRequeueLimit = int32(1000)
)

func (s *Server) requeueResolvedRunWaits(ctx context.Context, orgID pgtype.UUID) {
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	var rows []db.RequeueResolvedRunWaitsRow
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		rows, err = requeueResolvedRunWaitsWithStore(ctx, work.q, orgID, log)
		return err
	})
	if err != nil {
		log.Error("requeue resolved run waits failed", "org_id", pgvalue.UUIDString(orgID), "error", err)
		return
	}
	if s.runEnqueuer == nil {
		return
	}
	for _, row := range rows {
		if _, err := s.runEnqueuer.EnqueueRun(ctx, row.OrgID, row.RunID); err != nil && !errors.Is(err, dispatch.ErrNoEnqueueCandidate) {
			log.Error("enqueue resumed run failed", "org_id", pgvalue.UUIDString(row.OrgID), "run_id", pgvalue.UUIDString(row.RunID), "run_wait_id", pgvalue.UUIDString(row.ID), "error", err)
		}
	}
}

type runWaitResumeStore interface {
	RequeueResolvedRunWaits(context.Context, db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error)
	EnsureWorkspaceMountRequested(context.Context, db.EnsureWorkspaceMountRequestedParams) (db.EnsureWorkspaceMountRequestedRow, error)
	SetQueuedRunWorkspaceMount(context.Context, db.SetQueuedRunWorkspaceMountParams) error
}

type queuedRunWorkspaceMountStore interface {
	GetRun(context.Context, db.GetRunParams) (db.Run, error)
	EnsureWorkspaceMountRequested(context.Context, db.EnsureWorkspaceMountRequestedParams) (db.EnsureWorkspaceMountRequestedRow, error)
	SetQueuedRunWorkspaceMount(context.Context, db.SetQueuedRunWorkspaceMountParams) error
}

func requeueResolvedRunWaitsWithStore(ctx context.Context, store runWaitResumeStore, orgID pgtype.UUID, log *slog.Logger) ([]db.RequeueResolvedRunWaitsRow, error) {
	rows, err := store.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      orgID,
		LimitCount: runWaitRequeueLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		request, err := json.Marshal(map[string]string{
			"source":      "runtime_resume_wait",
			"run_id":      pgvalue.MustUUIDValue(row.RunID).String(),
			"run_wait_id": pgvalue.MustUUIDValue(row.ID).String(),
		})
		if err != nil {
			return nil, err
		}
		mount, err := ensureWorkspaceMountForQueuedRun(ctx, store, queuedRunWorkspaceMountTarget{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         row.OrgID,
			ProjectID:     row.ProjectID,
			EnvironmentID: row.EnvironmentID,
			WorkspaceID:   row.WorkspaceID,
			Priority:      row.Priority,
			Request:       request,
		})
		if err != nil {
			return nil, fmt.Errorf("ensure workspace mount for resumed run %s: %w", pgvalue.UUIDString(row.RunID), err)
		}
		if log != nil {
			log.Debug("queued run workspace mount ensured",
				"source", "runtime_resume_wait",
				"org_id", pgvalue.UUIDString(row.OrgID),
				"run_id", pgvalue.UUIDString(row.RunID),
				"run_wait_id", pgvalue.UUIDString(row.ID),
				"workspace_id", pgvalue.UUIDString(row.WorkspaceID),
				"workspace_mount_id", pgvalue.UUIDString(mount.ID),
				"state", mount.State,
				"priority", mount.Priority,
				"inserted", mount.Inserted,
				"decision", mount.Decision,
			)
		}
		if err := linkQueuedRunWorkspaceMount(ctx, store, queuedRunWorkspaceMountLink{
			OrgID:            row.OrgID,
			RunID:            row.RunID,
			WorkspaceID:      row.WorkspaceID,
			WorkspaceMountID: mount.ID,
		}); err != nil {
			return nil, fmt.Errorf("set workspace mount for resumed run %s: %w", pgvalue.UUIDString(row.RunID), err)
		}
	}
	return rows, nil
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
