package deployment

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// Position is the sort key of the last Deployment on a previous page.
type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// List returns up to limit Deployments of the environment after the given
// position, newest first, and whether more follow.
func List(ctx context.Context, q db.Querier, principal auth.Principal, scope auth.Scope, limit int32, after *Position) ([]db.ListScopedDeploymentsRow, bool, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return nil, false, err
	}
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return nil, false, err
	}
	params := db.ListScopedDeploymentsParams{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, RowLimit: limit + 1,
	}
	if after != nil {
		params.HasAfter = true
		params.AfterCreatedAt = pgvalue.Timestamptz(after.CreatedAt)
		params.AfterID = pgvalue.UUID(after.ID)
	}
	rows, err := q.ListScopedDeployments(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list deployments: %w", err)
	}
	if len(rows) > int(limit) {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// Get returns a Deployment of the environment.
func Get(ctx context.Context, q db.Querier, principal auth.Principal, scope auth.Scope, deploymentID uuid.UUID) (db.Deployment, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return db.Deployment{}, err
	}
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return db.Deployment{}, err
	}
	record, err := q.GetDeploymentForOrg(ctx, db.GetDeploymentForOrgParams{
		OrgID: pgvalue.UUID(principal.OrgID), ID: pgvalue.UUID(deploymentID),
	})
	if isNoRows(err) || (err == nil && (record.ProjectID != projectID || record.EnvironmentID != environmentID)) {
		return db.Deployment{}, ErrNotFound
	}
	if err != nil {
		return db.Deployment{}, fmt.Errorf("get deployment: %w", err)
	}
	return record, nil
}

// GetCurrent returns the environment's promoted Deployment.
func GetCurrent(ctx context.Context, q db.Querier, principal auth.Principal, scope auth.Scope) (db.Deployment, error) {
	if err := authorize(principal, scope, auth.PermissionRunsRead); err != nil {
		return db.Deployment{}, err
	}
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return db.Deployment{}, err
	}
	record, err := q.GetCurrentDeployment(ctx, db.GetCurrentDeploymentParams{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
	})
	if isNoRows(err) {
		return db.Deployment{}, ErrNoCurrentDeployment
	}
	if err != nil {
		return db.Deployment{}, fmt.Errorf("get current deployment: %w", err)
	}
	return record, nil
}

// DefinitionPage is one page of the definitions of one kind that a
// Deployment declares, ordered by declared ID.
type DefinitionPage struct {
	DeploymentID uuid.UUID
	DeclaredIDs  []string
	HasMore      bool
}

// ListDefinitions returns up to limit declared IDs of the given kind after
// afterDeclaredID, from the selected Deployment or, when selected is nil, the
// environment's current one.
func ListDefinitions(ctx context.Context, q db.Querier, principal auth.Principal, scope auth.Scope, kind definition.Kind, selected *uuid.UUID, limit int32, afterDeclaredID *string) (DefinitionPage, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return DefinitionPage{}, err
	}
	target, environmentID, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return DefinitionPage{}, err
	}
	params := db.ListDefinitionSnapshotsParams{
		EnvironmentID: environmentID, DeploymentID: target.ID, Kind: string(kind), RowLimit: limit + 1,
	}
	if afterDeclaredID != nil {
		params.HasAfter = true
		params.AfterID = *afterDeclaredID
	}
	declaredIDs, err := q.ListDefinitionSnapshots(ctx, params)
	if err != nil {
		return DefinitionPage{}, fmt.Errorf("list %s definitions: %w", kind, err)
	}
	page := DefinitionPage{DeploymentID: pgvalue.MustUUIDValue(target.ID), DeclaredIDs: declaredIDs}
	if len(declaredIDs) > int(limit) {
		page.DeclaredIDs, page.HasMore = declaredIDs[:limit], true
	}
	return page, nil
}

// GetDefinition returns the declared definition from the selected Deployment
// or, when selected is nil, the environment's current one, together with that
// Deployment's ID.
func GetDefinition(ctx context.Context, q db.Querier, principal auth.Principal, scope auth.Scope, kind definition.Kind, selected *uuid.UUID, declaredID string) (uuid.UUID, string, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return uuid.UUID{}, "", err
	}
	target, environmentID, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return uuid.UUID{}, "", err
	}
	declared, err := q.GetDefinitionSnapshot(ctx, db.GetDefinitionSnapshotParams{
		EnvironmentID: environmentID, DeploymentID: target.ID, Kind: string(kind), DeclaredID: declaredID,
	})
	if isNoRows(err) {
		return uuid.UUID{}, "", ErrDefinitionNotFound
	}
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("get definition: %w", err)
	}
	return pgvalue.MustUUIDValue(target.ID), declared, nil
}

// definitionDeployment resolves the Deployment whose definitions a read
// targets. Only a Deployment with a recorded Program has readable definitions.
func definitionDeployment(ctx context.Context, q db.Querier, principal auth.Principal, scope auth.Scope, selected *uuid.UUID) (db.Deployment, pgtype.UUID, error) {
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return db.Deployment{}, pgtype.UUID{}, err
	}
	orgID := pgvalue.UUID(principal.OrgID)
	var target db.Deployment
	if selected != nil {
		target, err = q.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, ID: pgvalue.UUID(*selected),
		})
		if isNoRows(err) {
			return db.Deployment{}, pgtype.UUID{}, ErrSelectedDeploymentNotFound
		}
	} else {
		target, err = q.GetCurrentDeploymentForRoute(ctx, db.GetCurrentDeploymentForRouteParams{
			OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID,
		})
		if isNoRows(err) {
			return db.Deployment{}, pgtype.UUID{}, ErrNoCurrentDefinitions
		}
	}
	if err != nil {
		return db.Deployment{}, pgtype.UUID{}, fmt.Errorf("resolve definition deployment: %w", err)
	}
	if !target.ProgramArtifactID.Valid || len(target.ProgramIndexDigest) == 0 || target.RuntimeArtifactDigest == "" {
		return db.Deployment{}, pgtype.UUID{}, ErrDefinitionsNotMaterialized
	}
	return target, environmentID, nil
}
