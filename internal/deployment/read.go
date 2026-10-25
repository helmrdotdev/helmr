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
	principal, refreshErr := currentPrincipal(ctx, q, principal, scope)
	if refreshErr != nil {
		return nil, false, refreshErr
	}
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
	principal, refreshErr := currentPrincipal(ctx, q, principal, scope)
	if refreshErr != nil {
		return Record{}, refreshErr
	}
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
	if err == nil {
		r.PreparationState, err = preparationState(ctx, q, r.EnvironmentID, id)
	}
	return r, err
}

func GetCurrent(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope) (Record, error) {
	principal, refreshErr := currentPrincipal(ctx, q, principal, scope)
	if refreshErr != nil {
		return Record{}, refreshErr
	}
	if !principal.HasPermission(auth.PermissionDeploymentsRead, scope) {
		return Record{}, ErrPermissionRequired
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
	ID          string
	AgentID     *uuid.UUID
	TemplateID  *string
	Resources   json.RawMessage
	InputKind   string
	InputSchema json.RawMessage
}

type DefinitionPage struct {
	DeploymentID uuid.UUID
	Items        []DefinitionRecord
	HasMore      bool
}

// Definition reads select one statement per kind so every statement the
// package sends is a constant.
const (
	agentDefinitionProjection    = "SELECT key, agent_id, template_key, NULL::jsonb, input_kind, input_schema FROM (SELECT d.environment_id,d.deployment_id,d.agent_id,a.key,t.key AS template_key,d.input_kind,d.input_schema FROM agent_definitions d JOIN agents a ON (a.environment_id,a.id)=(d.environment_id,d.agent_id) JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id)) definitions"
	templateDefinitionProjection = "SELECT key, NULL::uuid, NULL::text, resources, ''::text, NULL::jsonb FROM (SELECT d.environment_id,d.deployment_id,t.key,jsonb_strip_nulls(jsonb_build_object('milliCpu',d.cpu_millis,'memoryMiB',d.memory_bytes/1048576,'diskMiB',d.disk_bytes/1048576)) AS resources FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id)) definitions"
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

// Resolve only a known deployment identity within the addressed Environment.
// Definition callers separately authorize Agent or Template reads; enumerating
// deployment history requires deployments.read.
func definitionDeployment(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, selected *uuid.UUID) (Record, error) {
	project, env, err := scopeIDs(scope)
	if err != nil {
		return Record{}, err
	}
	var id uuid.UUID
	if selected != nil {
		id = *selected
	} else {
		err = q.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 AND current_deployment_id IS NOT NULL`, env, principal.OrgID, project).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, ErrNoCurrentDefinitions
		}
		if err != nil {
			return Record{}, err
		}
	}
	record, err := readRecord(ctx, q, pgvalue.MustUUIDValue(env), id)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (record.OrgID != principal.OrgID || record.ProjectID != pgvalue.MustUUIDValue(project)) {
		return Record{}, ErrSelectedDeploymentNotFound
	}
	return record, err
}

func ListDefinitions(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, kind definition.Kind, selected *uuid.UUID, limit int32, after *string) (DefinitionPage, error) {
	principal, refreshErr := currentPrincipal(ctx, q, principal, scope)
	if refreshErr != nil {
		return DefinitionPage{}, refreshErr
	}
	if limit < 1 || limit > 100 {
		return DefinitionPage{}, invalidInput(errors.New("definition page limit must be between 1 and 100"))
	}
	if err := validDefinitionKind(kind); err != nil {
		return DefinitionPage{}, err
	}
	allAgents, agentIDs := agentReadScope(principal, scope)
	allUse, useIDs := principal.AgentPermissionScope(auth.PermissionAgentsUse, scope)
	if kind == definition.KindTemplate && !principal.HasPermission(auth.PermissionTemplatesRead, scope) {
		return DefinitionPage{}, ErrPermissionRequired
	}

	r, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return DefinitionPage{}, err
	}
	var rows pgx.Rows
	if kind == definition.KindAgent {
		rows, err = q.Query(ctx, agentDefinitionProjection+" WHERE environment_id=$1 AND deployment_id=$2 AND ($3::text IS NULL OR key>$3) AND ($5::boolean OR agent_id=ANY($6::uuid[]) OR (deployment_id=(SELECT current_deployment_id FROM environments WHERE id=$1) AND ($7::boolean OR agent_id=ANY($8::uuid[])))) ORDER BY key LIMIT $4", r.EnvironmentID, r.ID, after, limit+1, allAgents, agentIDs, allUse, useIDs)
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
		if err := rows.Scan(&item.ID, &item.AgentID, &item.TemplateID, &item.Resources, &item.InputKind, &item.InputSchema); err != nil {
			return DefinitionPage{}, err
		}
		if item.AgentID != nil {
			target := scope
			target.AgentID = item.AgentID.String()
			if !principal.HasPermission(auth.PermissionAgentsRead, target) {
				item.TemplateID = nil
			}
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
	principal, refreshErr := currentPrincipal(ctx, q, principal, scope)
	if refreshErr != nil {
		return uuid.Nil(), DefinitionRecord{}, refreshErr
	}
	if err := validDefinitionKind(kind); err != nil {
		return uuid.Nil(), DefinitionRecord{}, err
	}
	allAgents, agentIDs := agentReadScope(principal, scope)
	allUse, useIDs := principal.AgentPermissionScope(auth.PermissionAgentsUse, scope)
	if kind == definition.KindTemplate && !principal.HasPermission(auth.PermissionTemplatesRead, scope) {
		return uuid.Nil(), DefinitionRecord{}, ErrPermissionRequired
	}

	r, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return uuid.Nil(), DefinitionRecord{}, err
	}
	var found DefinitionRecord
	var row pgx.Row
	if kind == definition.KindAgent {
		row = q.QueryRow(ctx, agentDefinitionProjection+definitionGetClause+" AND ($4::boolean OR agent_id=ANY($5::uuid[]) OR (deployment_id=(SELECT current_deployment_id FROM environments WHERE id=$1) AND ($6::boolean OR agent_id=ANY($7::uuid[]))))", r.EnvironmentID, r.ID, id, allAgents, agentIDs, allUse, useIDs)
	} else {
		row = q.QueryRow(ctx, templateDefinitionProjection+definitionGetClause, r.EnvironmentID, r.ID, id)
	}
	err = row.Scan(&found.ID, &found.AgentID, &found.TemplateID, &found.Resources, &found.InputKind, &found.InputSchema)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.ID, DefinitionRecord{}, ErrDefinitionNotFound
	}
	if err != nil {
		return uuid.Nil(), DefinitionRecord{}, err
	}
	if found.AgentID != nil {
		target := scope
		target.AgentID = found.AgentID.String()
		if !principal.HasPermission(auth.PermissionAgentsRead, target) {
			found.TemplateID = nil
		}
	}
	return r.ID, found, nil
}

// RetainedAgent describes a stable Agent independently of definition membership.
type RetainedAgent struct {
	DeletedAt           *time.Time
	Revision            int64
	Agent               string
	CurrentDeploymentID *string
	DefinedInCurrent    bool
}

// ListRetainedAgents returns up to limit+1 identities within the authorized scope.
// A nonempty key selects one identity; after is an exclusive key cursor.
func ListRetainedAgents(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, key, after string, limit int, deletedOnly bool) ([]RetainedAgent, error) {
	principal, refreshErr := currentPrincipal(ctx, q, principal, scope)
	if refreshErr != nil {
		return nil, refreshErr
	}
	allAgents, agentIDs := agentIdentityScope(principal, scope)

	if limit < 1 || limit > 100 {
		return nil, invalidInput(errors.New("agent page limit must be between 1 and 100"))
	}
	project, environment, err := scopeIDs(scope)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT a.key,e.current_deployment_id::text,a.deleted_at,a.revision,
 EXISTS(SELECT 1 FROM agent_definitions d WHERE d.environment_id=a.environment_id AND d.agent_id=a.id AND d.deployment_id=e.current_deployment_id)
 FROM agents a JOIN environments e ON e.id=a.environment_id
 WHERE e.org_id=$1 AND e.project_id=$2 AND e.id=$3
 AND (NOT $9::boolean OR a.deleted_at IS NOT NULL) AND ($4='' OR a.key=$4) AND ($5='' OR a.key>$5) AND ($7::boolean OR a.id=ANY($8::uuid[]))
 ORDER BY a.key LIMIT $6`, principal.OrgID, project, environment, key, after, limit+1, allAgents, agentIDs, deletedOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []RetainedAgent{}
	for rows.Next() {
		var item RetainedAgent
		if err := rows.Scan(&item.Agent, &item.CurrentDeploymentID, &item.DeletedAt, &item.Revision, &item.DefinedInCurrent); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
