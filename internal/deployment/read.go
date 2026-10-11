package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func (r Record) Version() string { return version(r.ID) }

// List returns finalized Deployments, newest first, within the authorized scope.
func List(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, limit int32, after *Position) ([]Record, bool, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return nil, false, err
	}
	if limit < 1 || limit > 100 {
		return nil, false, invalidInput(errors.New("deployment page limit must be between 1 and 100"))
	}
	project, env, err := scopeIDs(scope)
	if err != nil {
		return nil, false, err
	}
	var afterTime *time.Time
	var afterID *uuid.UUID
	if after != nil {
		afterTime = &after.CreatedAt
		afterID = &after.ID
	}
	rows, err := q.Query(ctx, `SELECT d.id,e.org_id,e.project_id,d.environment_id,d.bundle_digest,d.created_at FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE e.org_id=$1 AND e.project_id=$2 AND e.id=$3 AND ($4::timestamptz IS NULL OR (d.created_at,d.id)<($4,$5::uuid)) ORDER BY d.created_at DESC,d.id DESC LIMIT $6`, principal.OrgID, project, env, afterTime, afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.EnvironmentID, &r.BundleDigest, &r.CreatedAt); err != nil {
			return nil, false, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(result) > int(limit) {
		return result[:limit], true, nil
	}
	return result, false, nil
}

func Get(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, id uuid.UUID) (Record, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return Record{}, err
	}
	project, env, err := scopeIDs(scope)
	if err != nil {
		return Record{}, err
	}
	r, err := readRecord(ctx, q, pgvalue.MustUUIDValue(env), id)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (r.OrgID != principal.OrgID || r.ProjectID != pgvalue.MustUUIDValue(project)) {
		return Record{}, ErrNotFound
	}
	return r, err
}

func GetCurrent(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope) (Record, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return Record{}, err
	}
	project, env, err := scopeIDs(scope)
	if err != nil {
		return Record{}, err
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 AND current_deployment_id IS NOT NULL`, env, principal.OrgID, project).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNoCurrentDeployment
	}
	if err != nil {
		return Record{}, err
	}
	return readRecord(ctx, q, pgvalue.MustUUIDValue(env), id)
}

type DefinitionPage struct {
	DeploymentID uuid.UUID
	DeclaredIDs  []string
	HasMore      bool
}

func definitionTable(kind definition.Kind) (string, error) {
	switch kind {
	case definition.KindAgent:
		return "agent_definitions", nil
	case definition.KindComputer:
		return "computer_definitions", nil
	default:
		return "", invalidInput(fmt.Errorf("unsupported definition kind %q", kind))
	}
}
func definitionDeployment(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, selected *uuid.UUID) (Record, error) {
	if selected != nil {
		r, err := Get(ctx, q, principal, scope, *selected)
		if errors.Is(err, ErrNotFound) {
			err = ErrSelectedDeploymentNotFound
		}
		return r, err
	}
	r, err := GetCurrent(ctx, q, principal, scope)
	if errors.Is(err, ErrNoCurrentDeployment) {
		err = ErrNoCurrentDefinitions
	}
	return r, err
}
func ListDefinitions(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, kind definition.Kind, selected *uuid.UUID, limit int32, after *string) (DefinitionPage, error) {
	if limit < 1 || limit > 100 {
		return DefinitionPage{}, invalidInput(errors.New("definition page limit must be between 1 and 100"))
	}
	table, err := definitionTable(kind)
	if err != nil {
		return DefinitionPage{}, err
	}
	r, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return DefinitionPage{}, err
	}
	rows, err := q.Query(ctx, `SELECT definition_key FROM `+table+` WHERE environment_id=$1 AND deployment_id=$2 AND ($3::text IS NULL OR definition_key>$3) ORDER BY definition_key LIMIT $4`, r.EnvironmentID, r.ID, after, limit+1)
	if err != nil {
		return DefinitionPage{}, err
	}
	defer rows.Close()
	page := DefinitionPage{DeploymentID: r.ID, DeclaredIDs: []string{}}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return DefinitionPage{}, err
		}
		page.DeclaredIDs = append(page.DeclaredIDs, id)
	}
	if err := rows.Err(); err != nil {
		return DefinitionPage{}, err
	}
	if len(page.DeclaredIDs) > int(limit) {
		page.DeclaredIDs = page.DeclaredIDs[:limit]
		page.HasMore = true
	}
	return page, nil
}
func GetDefinition(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, kind definition.Kind, selected *uuid.UUID, id string) (uuid.UUID, string, error) {
	table, err := definitionTable(kind)
	if err != nil {
		return uuid.Nil(), "", err
	}
	r, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return uuid.Nil(), "", err
	}
	var found string
	err = q.QueryRow(ctx, `SELECT definition_key FROM `+table+` WHERE environment_id=$1 AND deployment_id=$2 AND definition_key=$3`, r.EnvironmentID, r.ID, id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrDefinitionNotFound
	}
	if err != nil {
		return uuid.Nil(), "", err
	}
	return r.ID, found, nil
}
