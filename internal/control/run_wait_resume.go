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

type queuedRunWorkspaceMaterializationStore interface {
	GetRun(context.Context, db.GetRunParams) (db.Run, error)
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
		materialization, err := ensureWorkspaceMaterializationForQueuedRun(ctx, store, queuedRunWorkspaceMaterializationTarget{
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
		if err := linkQueuedRunWorkspaceMaterialization(ctx, store, queuedRunWorkspaceMaterializationLink{
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

type queuedRunWorkspaceMaterializationTarget struct {
	ID            pgtype.UUID
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	WorkspaceID   pgtype.UUID
	Priority      int32
	Request       []byte
}

type queuedRunWorkspaceMaterializationLink struct {
	OrgID                      pgtype.UUID
	RunID                      pgtype.UUID
	WorkspaceID                pgtype.UUID
	WorkspaceMaterializationID pgtype.UUID
}

func ensureWorkspaceMaterializationForQueuedRun(ctx context.Context, store interface {
	EnsureWorkspaceMaterializationRequested(context.Context, db.EnsureWorkspaceMaterializationRequestedParams) (db.EnsureWorkspaceMaterializationRequestedRow, error)
}, target queuedRunWorkspaceMaterializationTarget) (db.EnsureWorkspaceMaterializationRequestedRow, error) {
	return store.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            target.ID,
		OrgID:         target.OrgID,
		ProjectID:     target.ProjectID,
		EnvironmentID: target.EnvironmentID,
		WorkspaceID:   target.WorkspaceID,
		Priority:      target.Priority,
		Request:       target.Request,
	})
}

func linkQueuedRunWorkspaceMaterialization(ctx context.Context, store interface {
	SetQueuedRunWorkspaceMaterialization(context.Context, db.SetQueuedRunWorkspaceMaterializationParams) error
}, link queuedRunWorkspaceMaterializationLink) error {
	return store.SetQueuedRunWorkspaceMaterialization(ctx, db.SetQueuedRunWorkspaceMaterializationParams{
		OrgID:                      link.OrgID,
		RunID:                      link.RunID,
		WorkspaceID:                link.WorkspaceID,
		WorkspaceMaterializationID: link.WorkspaceMaterializationID,
	})
}

func ensureQueuedRunWorkspaceMaterialization(ctx context.Context, store queuedRunWorkspaceMaterializationStore, orgID pgtype.UUID, runID pgtype.UUID, source string) (bool, error) {
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
	materialization, err := ensureWorkspaceMaterializationForQueuedRun(ctx, store, queuedRunWorkspaceMaterializationTarget{
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
	if err := linkQueuedRunWorkspaceMaterialization(ctx, store, queuedRunWorkspaceMaterializationLink{
		OrgID:                      run.OrgID,
		RunID:                      run.ID,
		WorkspaceID:                run.WorkspaceID,
		WorkspaceMaterializationID: materialization.ID,
	}); err != nil {
		return false, err
	}
	return true, nil
}
