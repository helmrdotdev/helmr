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

const runWaitRequeueLimit = int32(1000)

func (s *Server) requeueResolvedRunWaits(ctx context.Context, orgID pgtype.UUID) {
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	if s.tx == nil {
		log.Error("requeue resolved run waits requires transactional store", "org_id", pgvalue.UUIDString(orgID))
		return
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		log.Error("begin requeue resolved run waits failed", "org_id", pgvalue.UUIDString(orgID), "error", err)
		return
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	txStore := db.New(tx)
	rows, err := requeueResolvedRunWaitsWithStore(ctx, txStore, orgID)
	if err != nil {
		log.Error("requeue resolved run waits failed", "org_id", pgvalue.UUIDString(orgID), "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Error("commit requeue resolved run waits failed", "org_id", pgvalue.UUIDString(orgID), "error", err)
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
	EnsureWorkspaceMaterializationRequested(context.Context, db.EnsureWorkspaceMaterializationRequestedParams) (db.EnsureWorkspaceMaterializationRequestedRow, error)
	SetQueuedRunWorkspaceMaterialization(context.Context, db.SetQueuedRunWorkspaceMaterializationParams) error
}

func requeueResolvedRunWaitsWithStore(ctx context.Context, store runWaitResumeStore, orgID pgtype.UUID) ([]db.RequeueResolvedRunWaitsRow, error) {
	rows, err := store.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
		OrgID:      orgID,
		LimitCount: runWaitRequeueLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		request, err := json.Marshal(map[string]string{
			"source":      "run_wait_resume",
			"run_id":      pgvalue.MustUUIDValue(row.RunID).String(),
			"run_wait_id": pgvalue.MustUUIDValue(row.ID).String(),
		})
		if err != nil {
			return nil, err
		}
		materialization, err := store.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         row.OrgID,
			ProjectID:     row.ProjectID,
			EnvironmentID: row.EnvironmentID,
			WorkspaceID:   row.WorkspaceID,
			Priority:      row.Priority,
			Request:       request,
		})
		if err != nil {
			return nil, fmt.Errorf("ensure workspace materialization for resumed run %s: %w", pgvalue.UUIDString(row.RunID), err)
		}
		if err := store.SetQueuedRunWorkspaceMaterialization(ctx, db.SetQueuedRunWorkspaceMaterializationParams{
			OrgID:                      row.OrgID,
			RunID:                      row.RunID,
			WorkspaceID:                row.WorkspaceID,
			WorkspaceMaterializationID: materialization.ID,
		}); err != nil {
			return nil, fmt.Errorf("set workspace materialization for resumed run %s: %w", pgvalue.UUIDString(row.RunID), err)
		}
	}
	return rows, nil
}
