package workergroup

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

// ErrComputerPlacement rejects starting a Session on an existing Computer
// whose group is not the one its Agent's template placement resolves to. It is
// a kind of ErrCapacityIncompatible.
var ErrComputerPlacement error = placementError("computer runs on other capacity than this agent's template placement")

// PlacementUnassignedError rejects new work whose template placement has no
// assignment in its environment. It is a kind of ErrCapacityIncompatible.
type PlacementUnassignedError struct{ Placement string }

func (e PlacementUnassignedError) Error() string {
	return fmt.Sprintf("placement %q is not assigned in this environment", e.Placement)
}

func (PlacementUnassignedError) Unwrap() error { return ErrCapacityIncompatible }

type placementError string

func (e placementError) Error() string { return string(e) }

func (placementError) Unwrap() error { return ErrCapacityIncompatible }

// Target is the capacity work runs on: an organization group, or the
// deployment capacity of its environment's region when Group is nil.
type Target struct{ Group *uuid.UUID }

// Equal reports whether both targets name the same capacity.
func (t Target) Equal(other Target) bool {
	if t.Group == nil || other.Group == nil {
		return t.Group == nil && other.Group == nil
	}
	return *t.Group == *other.Group
}

// ResolvePlacement maps a template placement to its environment's current
// assignment. The default placement maps to deployment capacity unless it is
// reassigned; any other placement must be assigned.
func ResolvePlacement(ctx context.Context, q db.DBTX, env uuid.UUID, placement string) (Target, error) {
	var group *uuid.UUID
	err := q.QueryRow(ctx, `SELECT worker_group_id FROM environment_placements WHERE environment_id=$1 AND name=$2`, env, placement).Scan(&group)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && placement == definition.DefaultPlacement:
		return Target{}, nil
	case errors.Is(err, pgx.ErrNoRows):
		return Target{}, PlacementUnassignedError{Placement: placement}
	case err != nil:
		return Target{}, err
	}
	return Target{Group: group}, nil
}

// ResolveTemplatePlacement resolves the placement of a template definition.
func ResolveTemplatePlacement(ctx context.Context, q db.DBTX, env, deployment, template uuid.UUID) (Target, error) {
	var placement string
	if err := q.QueryRow(ctx, `SELECT placement FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2 AND template_id=$3`, env, deployment, template).Scan(&placement); err != nil {
		return Target{}, err
	}
	return ResolvePlacement(ctx, q, env, placement)
}

// ComputerTarget reads a Computer's pin. It is fixed at admission.
func ComputerTarget(ctx context.Context, q db.DBTX, env, computer uuid.UUID) (Target, error) {
	var t Target
	err := q.QueryRow(ctx, `SELECT worker_group_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&t.Group)
	return t, err
}

// Serves reports whether a group serves the target in an environment's region.
func (t Target) Serves(ctx context.Context, q db.DBTX, env, group uuid.UUID) (bool, error) {
	var serves bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e JOIN projects project ON project.id=e.project_id JOIN worker_groups g ON g.id=$2
 CROSS JOIN LATERAL (SELECT $3::uuid worker_group_id) target WHERE e.id=$1 AND `+db.ServesTargetSQL+`)`, env, group, t.Group).Scan(&serves)
	return serves, err
}
