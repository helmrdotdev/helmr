package control

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerRenewWorkspaceMountTransition(ctx context.Context, orgID string, workspaceMountID string) (db.WorkspaceMount, error) {
	params, err := s.workerRenewWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	row, err := s.db.RenewWorkspaceMount(ctx, params)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return db.WorkspaceMount(row), nil
}

func (s *Server) workerMarkWorkspaceMountMountedTransition(ctx context.Context, orgID string, workspaceMountID string) (db.WorkspaceMount, error) {
	params, err := s.workerMountedWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	var mount db.WorkspaceMount
	err = s.inTx(ctx, func(work *txWork) error {
		row, err := work.q.MarkWorkspaceMountMounted(ctx, params)
		if err != nil {
			return err
		}
		mount = db.WorkspaceMount(row)
		return enqueuePendingWorkspacePrimitiveOperations(ctx, work.q, mount)
	})
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return mount, nil
}

func (s *Server) workerStopWorkspaceMountTransition(ctx context.Context, orgID string, workspaceMountID string) (db.WorkspaceMount, error) {
	params, err := s.workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	row, err := s.db.StopWorkspaceMount(ctx, db.StopWorkspaceMountParams{
		ReasonCode:        pgtype.Text{String: "worker_unmounted", Valid: true},
		OrgID:             params.OrgID,
		ID:                params.ID,
		WorkerInstanceID:  params.WorkerInstanceID,
		WorkerEpoch:       params.WorkerEpoch,
		RuntimeInstanceID: params.RuntimeInstanceID,
		FencingGeneration: params.FencingGeneration,
	})
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return db.WorkspaceMount(row), nil
}

type workerWorkspaceMountTransitionIDs struct {
	OrgID             pgtype.UUID
	ID                pgtype.UUID
	WorkerInstanceID  pgtype.UUID
	WorkerEpoch       int64
	RuntimeInstanceID pgtype.UUID
	FencingGeneration int64
}

func (s *Server) workerRenewWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string) (db.RenewWorkspaceMountParams, error) {
	params, err := s.workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID)
	if err != nil {
		return db.RenewWorkspaceMountParams{}, err
	}
	return db.RenewWorkspaceMountParams{
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
		OrgID:                       params.OrgID,
		ID:                          params.ID,
		WorkerInstanceID:            params.WorkerInstanceID,
		WorkerEpoch:                 params.WorkerEpoch,
		RuntimeInstanceID:           params.RuntimeInstanceID,
	}, nil
}

func (s *Server) workerMountedWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string) (db.MarkWorkspaceMountMountedParams, error) {
	params, err := s.workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID)
	if err != nil {
		return db.MarkWorkspaceMountMountedParams{}, err
	}
	return db.MarkWorkspaceMountMountedParams{
		OrgID:             params.OrgID,
		ID:                params.ID,
		WorkerInstanceID:  params.WorkerInstanceID,
		WorkerEpoch:       params.WorkerEpoch,
		RuntimeInstanceID: params.RuntimeInstanceID,
		FencingGeneration: params.FencingGeneration,
	}, nil
}

func (s *Server) workerWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string) (workerWorkspaceMountTransitionIDs, error) {
	orgUUID, err := uuid.Parse(strings.TrimSpace(orgID))
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("org_id must be a UUID"))
	}
	id, err := uuid.Parse(strings.TrimSpace(workspaceMountID))
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("workspace_mount_id must be a UUID"))
	}
	worker := workerFromContext(ctx)
	mount, err := s.db.GetWorkspaceMountForWorkerTransition(ctx, db.GetWorkspaceMountForWorkerTransitionParams{
		OrgID: pgvalue.UUID(orgUUID), ID: pgvalue.UUID(id),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
	})
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, err
	}
	return workerWorkspaceMountTransitionIDs{
		OrgID: pgvalue.UUID(orgUUID), ID: pgvalue.UUID(id),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		RuntimeInstanceID: mount.RuntimeInstanceID, FencingGeneration: mount.FencingGeneration,
	}, nil
}
