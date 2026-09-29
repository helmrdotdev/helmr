package org

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

// ResolveEnvironmentScope resolves canonical project and environment IDs to
// the scope of an active environment of an active project in the
// organization. A malformed or unknown reference is an InputError.
func ResolveEnvironmentScope(ctx context.Context, q db.Querier, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, error) {
	parsedProjectID, err := ids.Parse(projectID)
	if err != nil {
		return auth.Scope{}, invalidInput("project_id must be a canonical project UUIDv7")
	}
	project, err := q.GetProject(ctx, db.GetProjectParams{OrgID: pgvalue.UUID(orgID), ID: pgvalue.UUID(parsedProjectID)})
	if isNoRows(err) {
		return auth.Scope{}, invalidInput("project_id must reference an active project")
	}
	if err != nil {
		return auth.Scope{}, fmt.Errorf("load project: %w", err)
	}
	parsedEnvironmentID, err := ids.Parse(environmentID)
	if err != nil {
		return auth.Scope{}, invalidInput("environment_id must be a canonical environment UUIDv7")
	}
	_, err = q.GetEnvironment(ctx, db.GetEnvironmentParams{OrgID: pgvalue.UUID(orgID), ProjectID: project.ID, ID: pgvalue.UUID(parsedEnvironmentID)})
	if isNoRows(err) {
		return auth.Scope{}, invalidInput("environment_id must reference an active environment")
	}
	if err != nil {
		return auth.Scope{}, fmt.Errorf("load environment: %w", err)
	}
	return auth.Scope{OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID}, nil
}
