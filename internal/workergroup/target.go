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

// GroupRetiredError rejects new work whose placement is assigned to a
// draining or disabled worker group, at once and before anything is queued:
// a Computer pinned there could never run. A paused group is only
// unavailable. It is a kind of ErrCapacityIncompatible.
type GroupRetiredError struct{ Placement, Status string }

func (e GroupRetiredError) Error() string {
	return fmt.Sprintf("placement %q is assigned to a %s worker group", e.Placement, e.Status)
}

func (GroupRetiredError) Unwrap() error { return ErrCapacityIncompatible }

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
// reassigned; any other placement must be assigned, and not to a draining
// or disabled group.
func ResolvePlacement(ctx context.Context, q db.DBTX, env uuid.UUID, placement string) (Target, error) {
	var group *uuid.UUID
	var status *string
	err := q.QueryRow(ctx, `SELECT p.worker_group_id,g.status::text FROM environment_placements p LEFT JOIN worker_groups g ON g.id=p.worker_group_id WHERE p.environment_id=$1 AND p.name=$2`, env, placement).Scan(&group, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && placement == definition.DefaultPlacement:
		return Target{}, nil
	case errors.Is(err, pgx.ErrNoRows):
		return Target{}, PlacementUnassignedError{Placement: placement}
	case err != nil:
		return Target{}, err
	case status != nil && (*status == "draining" || *status == "disabled"):
		return Target{}, GroupRetiredError{Placement: placement, Status: *status}
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

// ImageSpecTarget resolves the placement of a preparation spec. Only queued
// attempts are resolved; an attempt already assigned to a host is never
// retargeted.
func ImageSpecTarget(ctx context.Context, q db.DBTX, env, spec uuid.UUID) (Target, error) {
	var placement string
	if err := q.QueryRow(ctx, `SELECT placement FROM template_definitions WHERE environment_id=$1 AND image_spec_id=$2 ORDER BY deployment_id,template_id LIMIT 1`, env, spec).Scan(&placement); err != nil {
		return Target{}, err
	}
	return ResolvePlacement(ctx, q, env, placement)
}

// LockPreparationTarget holds the organization group a spec's placement
// resolves to while preparation for it is queued, and reports whether that
// group still accepts it. Retirement locks the group before it counts queued
// preparation, so the two serialize; preparation is never queued for a
// draining or disabled group.
func LockPreparationTarget(ctx context.Context, tx pgx.Tx, env, spec uuid.UUID) (bool, error) {
	var retired bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT $1::uuid environment_id,$2::uuid image_spec_id) prep JOIN LATERAL `+db.PreparationTargetSQL+` target ON true
 JOIN LATERAL (SELECT status FROM worker_groups g WHERE g.id=target.worker_group_id FOR SHARE) g ON true WHERE g.status IN ('draining','disabled'))`, env, spec).Scan(&retired)
	return !retired, err
}
