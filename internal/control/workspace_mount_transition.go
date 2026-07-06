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

func (s *Server) workerRenewWorkspaceMountTransition(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.WorkspaceMount, error) {
	params, err := workerRenewWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	row, err := s.db.RenewWorkspaceMount(ctx, params)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return db.WorkspaceMount(row), nil
}

func (s *Server) workerMarkWorkspaceMountMountedTransition(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.WorkspaceMount, error) {
	params, err := workerMountedWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
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

func (s *Server) workerStopWorkspaceMountTransition(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.WorkspaceMount, error) {
	params, err := workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	row, err := s.db.StopWorkspaceMount(ctx, db.StopWorkspaceMountParams{
		OrgID:                params.OrgID,
		ID:                   params.ID,
		WorkerInstanceID:     params.WorkerInstanceID,
		RuntimeInstanceToken: params.RuntimeInstanceToken,
	})
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return stoppedWorkspaceMount(row), nil
}

type workerWorkspaceMountTransitionIDs struct {
	OrgID                pgtype.UUID
	WorkerGroupID        string
	ID                   pgtype.UUID
	WorkerInstanceID     pgtype.UUID
	RuntimeInstanceToken string
}

func workerRenewWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.RenewWorkspaceMountParams, error) {
	params, err := workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.RenewWorkspaceMountParams{}, err
	}
	return db.RenewWorkspaceMountParams{
		OrgID:                       params.OrgID,
		ID:                          params.ID,
		WorkerInstanceID:            params.WorkerInstanceID,
		RuntimeInstanceToken:        params.RuntimeInstanceToken,
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
	}, nil
}

func workerMountedWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.MarkWorkspaceMountMountedParams, error) {
	params, err := workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.MarkWorkspaceMountMountedParams{}, err
	}
	return db.MarkWorkspaceMountMountedParams{
		OrgID:                       params.OrgID,
		ID:                          params.ID,
		WorkerInstanceID:            params.WorkerInstanceID,
		RuntimeInstanceToken:        params.RuntimeInstanceToken,
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
	}, nil
}

func workerWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (workerWorkspaceMountTransitionIDs, error) {
	orgUUID, err := uuid.Parse(strings.TrimSpace(orgID))
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("org_id must be a UUID"))
	}
	id, err := uuid.Parse(strings.TrimSpace(workspaceMountID))
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("workspace_mount_id must be a UUID"))
	}
	token := strings.TrimSpace(reservationToken)
	if token == "" {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("runtime_instance_token is required"))
	}
	worker := workerFromContext(ctx)
	return workerWorkspaceMountTransitionIDs{
		OrgID:                pgvalue.UUID(orgUUID),
		WorkerGroupID:        worker.WorkerGroupID,
		ID:                   pgvalue.UUID(id),
		WorkerInstanceID:     pgvalue.UUID(worker.WorkerInstanceID),
		RuntimeInstanceToken: token,
	}, nil
}
