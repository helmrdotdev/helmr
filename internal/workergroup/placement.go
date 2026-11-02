package workergroup

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrEnvironmentNotFound = errors.New("environment not found")
	// ErrPlacementGroup rejects assigning a placement to a group that is not
	// owned by the environment's organization in its project's region.
	ErrPlacementGroup = errors.New("placement worker group must be owned by the environment's organization in its region")
)

// Placement maps a placement name in an environment to its capacity.
// WorkerGroupID names the organization group, or is nil for the deployment
// capacity of the environment's region.
type Placement struct {
	Name          string
	WorkerGroupID *uuid.UUID
}

// ValidatePlacementName checks the definition ID syntax placement names share.
func ValidatePlacementName(name string) error {
	if !definition.ValidDeclaredID(name) {
		return invalidInput("placement name must start with a letter or digit and contain up to 128 letters, digits, '.', '_' or '-'")
	}
	return nil
}

// SetPlacement assigns a placement name in an environment. Assigning the
// default placement to deployment capacity removes its override.
func SetPlacement(ctx context.Context, q db.Querier, environmentID uuid.UUID, placement Placement) error {
	if err := ValidatePlacementName(placement.Name); err != nil {
		return err
	}
	if placement.Name == definition.DefaultPlacement && placement.WorkerGroupID == nil {
		_, err := RemovePlacement(ctx, q, environmentID, placement.Name)
		return err
	}
	params := db.SetEnvironmentPlacementParams{EnvironmentID: pgvalue.UUID(environmentID), Name: placement.Name}
	if placement.WorkerGroupID != nil {
		params.WorkerGroupID = pgvalue.UUID(*placement.WorkerGroupID)
	}
	_, err := q.SetEnvironmentPlacement(ctx, params)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrEnvironmentNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "environment_placements_group_fkey":
		return ErrPlacementGroup
	case err != nil:
		return fmt.Errorf("set placement %q: %w", placement.Name, err)
	}
	return nil
}

// RemovePlacement removes a placement name from an environment and reports
// whether it was assigned. Removing the default placement resets it to
// deployment capacity.
func RemovePlacement(ctx context.Context, q db.Querier, environmentID uuid.UUID, name string) (bool, error) {
	if err := ValidatePlacementName(name); err != nil {
		return false, err
	}
	removed, err := q.DeleteEnvironmentPlacement(ctx, db.DeleteEnvironmentPlacementParams{EnvironmentID: pgvalue.UUID(environmentID), Name: name})
	if err != nil {
		return false, fmt.Errorf("remove placement %q: %w", name, err)
	}
	return removed > 0, nil
}

// ListPlacements returns an environment's placements by name, including the
// default placement when it maps to deployment capacity.
func ListPlacements(ctx context.Context, q db.Querier, environmentID uuid.UUID) ([]Placement, error) {
	rows, err := q.ListEnvironmentPlacements(ctx, pgvalue.UUID(environmentID))
	if err != nil {
		return nil, fmt.Errorf("list placements: %w", err)
	}
	placements := make([]Placement, 0, len(rows)+1)
	defaulted := false
	for _, row := range rows {
		if !defaulted && row.Name > definition.DefaultPlacement {
			placements = append(placements, Placement{Name: definition.DefaultPlacement})
			defaulted = true
		}
		placement := Placement{Name: row.Name}
		if row.WorkerGroupID.Valid {
			group := pgvalue.MustUUIDValue(row.WorkerGroupID)
			placement.WorkerGroupID = &group
		}
		defaulted = defaulted || row.Name == definition.DefaultPlacement
		placements = append(placements, placement)
	}
	if !defaulted {
		placements = append(placements, Placement{Name: definition.DefaultPlacement})
	}
	return placements, nil
}
