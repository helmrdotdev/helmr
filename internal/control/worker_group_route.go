package control

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerGroupPlacementResolver interface {
	GetWorkerGroupPlacementForRecord(context.Context, string) (db.GetWorkerGroupPlacementForRecordRow, error)
}

func (s *Server) requireEnvironmentPlacementWorkerGroup(ctx context.Context, store environmentPlacementStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (string, error) {
	placement, err := s.resolveEnvironmentPlacement(ctx, store, orgID, projectID, environmentID)
	if err != nil {
		return "", err
	}
	return placement.WorkerGroupID, nil
}

func (s *Server) requireRoutableRecordWorkerGroup(ctx context.Context, store workerGroupPlacementResolver, recordWorkerGroupID string) error {
	if _, err := store.GetWorkerGroupPlacementForRecord(ctx, recordWorkerGroupID); isNoRows(err) {
		return unavailable(errors.New("record placement is not available"))
	} else if err != nil {
		return err
	}
	return nil
}
