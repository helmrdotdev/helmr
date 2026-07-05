package control

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type environmentCellRouteResolver interface {
	GetEnvironmentCellRouteForRecord(context.Context, db.GetEnvironmentCellRouteForRecordParams) (db.GetEnvironmentCellRouteForRecordRow, error)
	GetEnvironmentCellRouteForRecordGeneration(context.Context, db.GetEnvironmentCellRouteForRecordGenerationParams) (db.GetEnvironmentCellRouteForRecordGenerationRow, error)
}

func (s *Server) requireRoutableEnvironmentCell(ctx context.Context, store environmentPlacementStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (string, error) {
	placement, err := s.resolveEnvironmentPlacement(ctx, store, orgID, projectID, environmentID)
	if err != nil {
		return "", err
	}
	return placement.CellID, nil
}

func (s *Server) requireRoutableRecordCell(ctx context.Context, store environmentCellRouteResolver, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, recordCellID string) error {
	if recordCellID != s.cellID {
		return unavailable(errors.New("record route cell mismatch"))
	}
	if _, err := store.GetEnvironmentCellRouteForRecord(ctx, db.GetEnvironmentCellRouteForRecordParams{
		OrgID:         pgvalue.UUID(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		CellID:        recordCellID,
	}); isNoRows(err) {
		return unavailable(errors.New("record route is not available"))
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) requireRoutableRecordCellGeneration(ctx context.Context, store environmentCellRouteResolver, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, recordCellID string, routeGeneration int64) error {
	if routeGeneration <= 0 {
		return unavailable(errors.New("record route generation is invalid"))
	}
	if recordCellID != s.cellID {
		return unavailable(errors.New("record route cell mismatch"))
	}
	if _, err := store.GetEnvironmentCellRouteForRecordGeneration(ctx, db.GetEnvironmentCellRouteForRecordGenerationParams{
		OrgID:           pgvalue.UUID(orgID),
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		CellID:          recordCellID,
		RouteGeneration: routeGeneration,
	}); isNoRows(err) {
		return unavailable(errors.New("record route generation is not available"))
	} else if err != nil {
		return err
	}
	return nil
}
