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
		rows, err = requeueResolvedRunWaitsWithStore(ctx, work.q, orgID, s.cellID, log)
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

func requeueResolvedRunWaitsWithStore(ctx context.Context, store runWaitResumeStore, orgID pgtype.UUID, cellID string, log *slog.Logger) ([]db.RequeueResolvedRunWaitsRow, error) {
	rows, err := store.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      orgID,
		CellID:     cellID,
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
			CellID:        row.CellID,
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
