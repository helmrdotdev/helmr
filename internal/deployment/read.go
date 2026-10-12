package deployment

import (
	"context"
	"encoding/json"
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

type DefinitionRecord struct {
	ID         string
	AgentID    *uuid.UUID
	TemplateID *string
	Resources  json.RawMessage
}

type DefinitionPage struct {
	DeploymentID uuid.UUID
	Items        []DefinitionRecord
	HasMore      bool
}

// Definition reads select one statement per kind so every statement the
// package sends is a constant.
const (
	agentDefinitionProjection    = "SELECT key, agent_id, template_key, NULL::jsonb FROM (SELECT d.environment_id,d.deployment_id,d.agent_id,a.key,t.key AS template_key FROM agent_definitions d JOIN agents a ON (a.environment_id,a.id)=(d.environment_id,d.agent_id) JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id)) definitions"
	templateDefinitionProjection = "SELECT key, NULL::uuid, NULL::text, resources FROM (SELECT d.environment_id,d.deployment_id,t.key,jsonb_strip_nulls(jsonb_build_object('milliCpu',d.cpu_millis,'memoryMiB',d.memory_bytes/1048576,'diskMiB',d.disk_bytes/1048576)) AS resources FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id)) definitions"
	definitionListClause         = " WHERE environment_id=$1 AND deployment_id=$2 AND ($3::text IS NULL OR key>$3) ORDER BY key LIMIT $4"
	definitionGetClause          = " WHERE environment_id=$1 AND deployment_id=$2 AND key=$3"
)

func validDefinitionKind(kind definition.Kind) error {
	switch kind {
	case definition.KindAgent, definition.KindTemplate:
		return nil
	}
	return invalidInput(fmt.Errorf("unsupported definition kind %q", kind))
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
	if err := validDefinitionKind(kind); err != nil {
		return DefinitionPage{}, err
	}
	r, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return DefinitionPage{}, err
	}
	var rows pgx.Rows
	if kind == definition.KindAgent {
		rows, err = q.Query(ctx, agentDefinitionProjection+definitionListClause, r.EnvironmentID, r.ID, after, limit+1)
	} else {
		rows, err = q.Query(ctx, templateDefinitionProjection+definitionListClause, r.EnvironmentID, r.ID, after, limit+1)
	}
	if err != nil {
		return DefinitionPage{}, err
	}
	defer rows.Close()
	page := DefinitionPage{DeploymentID: r.ID, Items: []DefinitionRecord{}}
	for rows.Next() {
		var item DefinitionRecord
		if err := rows.Scan(&item.ID, &item.AgentID, &item.TemplateID, &item.Resources); err != nil {
			return DefinitionPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return DefinitionPage{}, err
	}
	if len(page.Items) > int(limit) {
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	return page, nil
}
func GetDefinition(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, kind definition.Kind, selected *uuid.UUID, id string) (uuid.UUID, DefinitionRecord, error) {
	if err := validDefinitionKind(kind); err != nil {
		return uuid.Nil(), DefinitionRecord{}, err
	}
	r, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return uuid.Nil(), DefinitionRecord{}, err
	}
	var found DefinitionRecord
	var row pgx.Row
	if kind == definition.KindAgent {
		row = q.QueryRow(ctx, agentDefinitionProjection+definitionGetClause, r.EnvironmentID, r.ID, id)
	} else {
		row = q.QueryRow(ctx, templateDefinitionProjection+definitionGetClause, r.EnvironmentID, r.ID, id)
	}
	err = row.Scan(&found.ID, &found.AgentID, &found.TemplateID, &found.Resources)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.ID, DefinitionRecord{}, ErrDefinitionNotFound
	}
	if err != nil {
		return uuid.Nil(), DefinitionRecord{}, err
	}
	return r.ID, found, nil
}

// RetainedAgent describes a stable Agent independently of definition membership.
type RetainedAgent struct {
	Agent               string
	CurrentDeploymentID *string
	DefinedInCurrent    bool
}

// ListRetainedAgents returns up to limit+1 identities within the authorized scope.
// A nonempty key selects one identity; after is an exclusive key cursor.
func ListRetainedAgents(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, key, after string, limit int) ([]RetainedAgent, error) {
	if err := authorizeRead(principal, scope); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, invalidInput(errors.New("agent page limit must be between 1 and 100"))
	}
	project, environment, err := scopeIDs(scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT a.key,e.current_deployment_id::text,
 EXISTS(SELECT 1 FROM agent_definitions d WHERE d.environment_id=a.environment_id AND d.agent_id=a.id AND d.deployment_id=e.current_deployment_id)
 FROM agents a JOIN environments e ON e.id=a.environment_id
 WHERE e.org_id=$1 AND e.project_id=$2 AND e.id=$3
 AND ($4='' OR a.key=$4) AND ($5='' OR a.key>$5)
 ORDER BY a.key LIMIT $6`, principal.OrgID, project, environment, key, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []RetainedAgent{}
	for rows.Next() {
		var item RetainedAgent
		if err := rows.Scan(&item.Agent, &item.CurrentDeploymentID, &item.DefinedInCurrent); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
