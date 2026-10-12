package org

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// ResolveEnvironmentScope resolves canonical project and environment IDs to
// the scope of an active environment of an active project in the
// organization. A malformed or unknown reference is an InputError.
func ResolveEnvironmentScope(ctx context.Context, q db.Querier, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, error) {
	project, err := GetProject(ctx, q, orgID, projectID)
	if errors.Is(err, ErrProjectNotFound) {
		return auth.Scope{}, invalidInput("project must reference an active project")
	}
	if err != nil {
		return auth.Scope{}, err
	}
	environment, err := GetEnvironmentByReference(ctx, q, orgID, pgvalue.MustUUIDValue(project.ID), environmentID)
	if errors.Is(err, ErrEnvironmentNotFound) {
		return auth.Scope{}, invalidInput("environment must reference an active environment")
	}
	if err != nil {
		return auth.Scope{}, err
	}
	return auth.Scope{OrgID: orgID, ProjectID: pgvalue.UUIDString(project.ID), EnvironmentID: pgvalue.UUIDString(environment.ID)}, nil
}
