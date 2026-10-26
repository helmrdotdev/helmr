package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type TemplateBuild struct {
	DeploymentID uuid.UUID `json:"deployment_id"`
	TemplateID   string    `json:"template_id"`
	ImageSpecID  uuid.UUID `json:"image_spec_id"`
	ImageBuildID uuid.UUID `json:"image_build_id"`
	Status       string    `json:"status"`
	Ready        bool      `json:"ready"`
	Deadline     time.Time `json:"deadline"`
	ErrorCode    string    `json:"error_code,omitempty"`
}

// RefreshTemplate requests unfinished preparation for the current Template.
// Existing Computers and the last eligible image retain their current pins.
func RefreshTemplate(ctx context.Context, auditPolicy auditpolicy.Policy,

	database db.TxBeginner, principal auth.Principal, scope auth.Scope, template, key string) (TemplateBuild, error) {
	if err := authorize(principal, scope, auth.PermissionTemplatesRefresh); err != nil {
		return TemplateBuild{}, err
	}
	if !definition.ValidDeclaredID(template) {
		return TemplateBuild{}, invalidInput(errors.New("invalid Template"))
	}
	project, environment, err := scopeIDs(scope)
	if err != nil {
		return TemplateBuild{}, err
	}
	env := pgvalue.MustUUIDValue(environment)
	request, err := idempotency.NewTemplateRefreshRequest(env, template, key)
	if err != nil {
		return TemplateBuild{}, invalidInput(err)
	}
	var result TemplateBuild
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := authorizeCurrent(ctx, tx, principal, scope, auth.PermissionTemplatesRefresh); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 FOR NO KEY UPDATE`, env, principal.OrgID, project).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := authorizeCurrent(ctx, tx, principal, scope, auth.PermissionTemplatesRefresh); err != nil {
			return err
		}
		err := idempotency.Run(ctx, tx, request, func(tx pgx.Tx, acquired idempotency.Result) (idempotency.Completion, error) {
			if !acquired.New {
				if err := json.Unmarshal(acquired.Claim.Receipt, &result); err != nil {
					return idempotency.Completion{}, err
				}
				if result.TemplateID != template || result.DeploymentID != pgvalue.MustUUIDValue(acquired.Claim.DeploymentID) {
					return idempotency.Completion{}, errors.New("refresh receipt does not match its request")
				}
				return idempotency.Completion{}, nil
			}
			result.TemplateID = template
			if err := tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND current_deployment_id IS NOT NULL`, env).Scan(&result.DeploymentID); errors.Is(err, pgx.ErrNoRows) {
				return idempotency.Completion{}, ErrNoCurrentDeployment
			} else if err != nil {
				return idempotency.Completion{}, err
			}
			var prepare bool
			var templateID uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT d.image_spec_id,d.prepare,d.template_id FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) WHERE d.environment_id=$1 AND d.deployment_id=$2 AND t.key=$3`, env, result.DeploymentID, template).Scan(&result.ImageSpecID, &prepare, &templateID); errors.Is(err, pgx.ErrNoRows) {
				return idempotency.Completion{}, ErrDefinitionNotFound
			} else if err != nil {
				return idempotency.Completion{}, err
			}
			if !prepare {
				return idempotency.Completion{}, invalidInput(errors.New("template does not define preparation"))
			}
			if err := computer.LockImageSpecSecrets(ctx, tx, env, result.ImageSpecID); err != nil {
				return idempotency.Completion{}, err
			}
			if err := computer.LockImageSpec(ctx, tx, env, result.ImageSpecID); err != nil {
				return idempotency.Completion{}, err
			}
			if err := authorizeCurrent(ctx, tx, principal, scope, auth.PermissionTemplatesRefresh); err != nil {
				return idempotency.Completion{}, err
			}
			build, err := computer.StartTemplateRefresh(ctx, tx, env, result.ImageSpecID, result.DeploymentID)
			if err != nil {
				return idempotency.Completion{}, err
			}
			result.ImageBuildID = build
			if err := readTemplateBuildState(ctx, tx, env, &result); err != nil {
				return idempotency.Completion{}, err
			}
			current, err := currentPrincipal(ctx, tx, principal, scope)
			if err != nil {
				return idempotency.Completion{}, err
			}
			actor, err := audit.Authorized(current, auth.PermissionTemplatesRefresh, scope)
			if err != nil {
				return idempotency.Completion{}, err
			}
			if err := audit.Append(ctx, tx, auditPolicy, audit.Event{Evidence: actor, EnvironmentID: env, DeploymentID: result.DeploymentID, Action: "template.refresh_requested", Outcome: "accepted", TargetKind: "template", TargetID: templateID, Details: audit.TemplateRefreshDetails{ImageBuildID: build}}); err != nil {
				return idempotency.Completion{}, err
			}
			receipt, err := json.Marshal(result)
			return idempotency.Completion{Target: idempotency.Target{DeploymentID: result.DeploymentID}, Receipt: receipt}, err
		})
		if err != nil {
			return err
		}
		return readTemplateBuildState(ctx, tx, env, &result)
	})
	return result, err
}

// ReadTemplateBuild observes the requested immutable build without requesting work.
func ReadTemplateBuild(ctx context.Context, q db.DBTX, principal auth.Principal, scope auth.Scope, template string, selected *uuid.UUID, build uuid.UUID) (TemplateBuild, error) {
	if !definition.ValidDeclaredID(template) || build == uuid.Nil() {
		return TemplateBuild{}, invalidInput(errors.New("invalid Template build"))
	}
	if err := authorizeCurrent(ctx, q, principal, scope, auth.PermissionTemplatesRead); err != nil {
		return TemplateBuild{}, err
	}
	deployment, err := definitionDeployment(ctx, q, principal, scope, selected)
	if err != nil {
		return TemplateBuild{}, err
	}
	result := TemplateBuild{DeploymentID: deployment.ID, TemplateID: template, ImageBuildID: build}
	err = q.QueryRow(ctx, `SELECT d.image_spec_id FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) JOIN image_builds b ON (b.environment_id,b.image_spec_id)=(d.environment_id,d.image_spec_id) WHERE d.environment_id=$1 AND d.deployment_id=$2 AND t.key=$3 AND b.id=$4`, deployment.EnvironmentID, deployment.ID, template, build).Scan(&result.ImageSpecID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TemplateBuild{}, ErrDefinitionNotFound
	}
	if err != nil {
		return TemplateBuild{}, err
	}
	err = readTemplateBuildState(ctx, q, deployment.EnvironmentID, &result)
	return result, err
}

func readTemplateBuildState(ctx context.Context, q db.DBTX, env uuid.UUID, result *TemplateBuild) error {
	var maxAge *int64
	if err := q.QueryRow(ctx, `SELECT d.max_image_age_ms FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) WHERE d.environment_id=$1 AND d.deployment_id=$2 AND d.image_spec_id=$3 AND t.key=$4`, env, result.DeploymentID, result.ImageSpecID, result.TemplateID).Scan(&maxAge); err != nil {
		return err
	}
	state, err := computer.ReadImageBuildState(ctx, q, env, result.ImageBuildID, maxAge)
	if err != nil {
		return err
	}
	if state.Build.SpecID != result.ImageSpecID {
		return errors.New("preparation build changed specification")
	}
	result.Status, result.Deadline, result.ErrorCode, result.Ready = state.Build.Status, state.Build.Deadline, state.ErrorCode, state.Ready
	return nil
}
