package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	workergrouppkg "github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

type environmentPlacementStore interface {
	workergrouppkg.PlacementStore
	GetEnvironment(context.Context, db.GetEnvironmentParams) (db.Environment, error)
}

func (s *Server) resolveEnvironmentPlacement(ctx context.Context, store environmentPlacementStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (workergrouppkg.Placement, error) {
	if _, err := store.GetEnvironment(ctx, db.GetEnvironmentParams{
		OrgID:     pgvalue.UUID(orgID),
		ProjectID: projectID,
		ID:        environmentID,
	}); err != nil {
		if isNoRows(err) {
			return workergrouppkg.Placement{}, notFound(errors.New("environment not found"))
		}
		return workergrouppkg.Placement{}, fmt.Errorf("load environment placement: %w", err)
	}
	placement, err := workergrouppkg.ResolvePlacement(ctx, store, workergrouppkg.ResolvePlacementParams{
		OrgID:     pgvalue.UUID(orgID),
		ProjectID: projectID,
	})
	if err == nil {
		return placement, nil
	}
	if errors.Is(err, workergrouppkg.ErrPlacementUnavailable) {
		return workergrouppkg.Placement{}, unavailable(errors.New("environment placement is unavailable"))
	}
	return workergrouppkg.Placement{}, fmt.Errorf("resolve environment placement: %w", err)
}
