package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// default placement to deployment capacity removes its override. It holds the
// environment lock promotion holds and refuses an assignment that leaves the
// current deployment with an unbound placement.
func SetPlacement(ctx context.Context, txb db.TxBeginner, environmentID uuid.UUID, placement Placement) error {
	if err := ValidatePlacementName(placement.Name); err != nil {
		return err
	}
	if placement.Name == definition.DefaultPlacement && placement.WorkerGroupID == nil {
		_, err := RemovePlacement(ctx, txb, environmentID, placement.Name)
		return err
	}
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		current, err := lockPlacements(ctx, tx, environmentID)
		if err != nil {
			return err
		}
		params := db.SetEnvironmentPlacementParams{EnvironmentID: pgvalue.UUID(environmentID), Name: placement.Name}
		if placement.WorkerGroupID != nil {
			params.WorkerGroupID = pgvalue.UUID(*placement.WorkerGroupID)
		}
		_, err = db.New(tx).SetEnvironmentPlacement(ctx, params)
		var pgErr *pgconn.PgError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrEnvironmentNotFound
		case errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "environment_placements_group_fkey":
			return ErrPlacementGroup
		case err != nil:
			return fmt.Errorf("set placement %q: %w", placement.Name, err)
		}
		return requireCurrentPlacements(ctx, tx, environmentID, current)
	})
}

// RemovePlacement removes a placement name from an environment and reports
// whether it was assigned. Removing the default placement resets it to
// deployment capacity; removing another placement the current deployment
// uses is refused.
func RemovePlacement(ctx context.Context, txb db.TxBeginner, environmentID uuid.UUID, name string) (bool, error) {
	if err := ValidatePlacementName(name); err != nil {
		return false, err
	}
	var removed int64
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		current, err := lockPlacements(ctx, tx, environmentID)
		if err != nil {
			return err
		}
		removed, err = db.New(tx).DeleteEnvironmentPlacement(ctx, db.DeleteEnvironmentPlacementParams{EnvironmentID: pgvalue.UUID(environmentID), Name: name})
		if err != nil {
			return fmt.Errorf("remove placement %q: %w", name, err)
		}
		return requireCurrentPlacements(ctx, tx, environmentID, current)
	})
	return removed > 0, err
}

// lockPlacements takes the environment lock promotion holds and returns the
// current deployment, if any.
func lockPlacements(ctx context.Context, tx pgx.Tx, environmentID uuid.UUID) (*uuid.UUID, error) {
	var current *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, environmentID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEnvironmentNotFound
	}
	return current, err
}

func requireCurrentPlacements(ctx context.Context, tx pgx.Tx, environmentID uuid.UUID, current *uuid.UUID) error {
	if current == nil {
		return nil
	}
	return RequireDeploymentPlacements(ctx, tx, environmentID, *current)
}

// UnboundPlacementsError names the placements a deployment uses that have no
// assignment in its environment or are assigned to a draining or disabled
// group.
type UnboundPlacementsError struct{ Unassigned, Retired []string }

func (e UnboundPlacementsError) Error() string {
	var parts []string
	if len(e.Unassigned) > 0 {
		parts = append(parts, "unassigned placements: "+strings.Join(e.Unassigned, ", "))
	}
	if len(e.Retired) > 0 {
		parts = append(parts, "placements assigned to a draining or disabled worker group: "+strings.Join(e.Retired, ", "))
	}
	return "deployment placements are not bound in this environment; " + strings.Join(parts, "; ")
}

// RequireDeploymentPlacements checks that every placement a deployment's
// templates use is bound in its environment to deployment capacity or to a
// group that is not draining or disabled, and holds those groups until the
// caller commits, so a group cannot retire under a check that passed. Image
// platform needs no check in this release: bundles admit only linux/x86_64
// images, which every VM platform runs. The caller holds the environment lock;
// retirement never takes it.
func RequireDeploymentPlacements(ctx context.Context, q db.DBTX, environmentID, deploymentID uuid.UUID) error {
	rows, err := q.Query(ctx, `SELECT used.placement,p.name IS NULL AND used.placement<>$3,COALESCE(g.status IN ('draining','disabled'),false)
 FROM (SELECT DISTINCT placement FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2) used
 LEFT JOIN environment_placements p ON (p.environment_id,p.name)=($1,used.placement)
 LEFT JOIN LATERAL (SELECT status FROM worker_groups WHERE id=p.worker_group_id FOR SHARE) g ON true
 ORDER BY used.placement COLLATE "C"`, environmentID, deploymentID, definition.DefaultPlacement)
	if err != nil {
		return fmt.Errorf("read deployment placements: %w", err)
	}
	var unbound UnboundPlacementsError
	var name string
	var unassigned, retired bool
	if _, err := pgx.ForEachRow(rows, []any{&name, &unassigned, &retired}, func() error {
		if unassigned {
			unbound.Unassigned = append(unbound.Unassigned, name)
		}
		if retired {
			unbound.Retired = append(unbound.Retired, name)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read deployment placements: %w", err)
	}
	if len(unbound.Unassigned) > 0 || len(unbound.Retired) > 0 {
		return unbound
	}
	return nil
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
