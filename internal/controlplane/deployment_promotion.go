package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type scheduleReconciliation struct {
	definition db.DeploymentDefinition
	manifest   deployment.ScheduleManifest
	placements []workspace.SecretPlacement
	nextFireAt time.Time
	record     db.ReconcileScheduleRow
}

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
	var request api.PromoteDeploymentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid deployment promotion request: %w", err)))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	principal, err := auth.ActorPrincipal(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if len(reason) > 1024 {
		writeError(w, badRequest(errors.New("promotion reason exceeds 1024 bytes")))
		return
	}
	effectiveFrom := time.Now().UTC()
	err = s.inTx(r.Context(), func(work *txWork) error {
		target, err := work.q.LockDeploymentPromotionTarget(
			r.Context(),
			db.LockDeploymentPromotionTargetParams{
				OrgID:         pgvalue.UUID(actor.OrgID),
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				DeploymentID:  pgvalue.UUID(deploymentID),
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound(errors.New("deployment not found or is not deployable"))
		}
		if err != nil {
			return fmt.Errorf("lock deployment promotion target: %w", err)
		}
		if err := reconcileSchedules(
			r.Context(),
			work.q,
			target,
			effectiveFrom,
		); err != nil {
			return err
		}
		if _, err := work.q.PromoteDeployment(
			r.Context(),
			db.PromoteDeploymentParams{
				ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:               target.OrgID,
				ProjectID:           target.ProjectID,
				EnvironmentID:       target.EnvironmentID,
				DeploymentID:        target.ID,
				PromotedByPrincipal: principal,
				Reason:              reason,
			},
		); err != nil {
			return fmt.Errorf("promote deployment: %w", err)
		}
		if err := appendDeploymentLifecycleEvent(
			r.Context(),
			work.q,
			target.OrgID,
			target.ProjectID,
			target.EnvironmentID,
			target.ID,
			"deployment.promoted",
			"info",
			"control",
			"promoted",
			"Deployment promoted",
		); err != nil {
			return fmt.Errorf("record deployment promotion event: %w", err)
		}
		return nil
	})
	if err != nil {
		writeDeploymentError(w, s, err)
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment storage is not configured")))
		return
	}
	record, err := store.GetDeployment(r.Context(), db.GetDeploymentParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(deploymentID),
	})
	if err != nil {
		writeError(w, fmt.Errorf("get promoted deployment: %w", err))
		return
	}
	response, err := deploymentResponseWithArtifacts(r.Context(), store, record)
	if err != nil {
		writeError(w, fmt.Errorf("get promoted deployment artifacts: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func reconcileSchedules(
	ctx context.Context,
	store db.Querier,
	target db.Deployment,
	effectiveFrom time.Time,
) error {
	definitions, err := store.ListDeploymentDefinitionsForDeployment(
		ctx,
		db.ListDeploymentDefinitionsForDeploymentParams{
			EnvironmentID: target.EnvironmentID,
			DeploymentID:  target.ID,
		},
	)
	if err != nil {
		return fmt.Errorf("list deployment definitions for schedule reconciliation: %w", err)
	}
	sandboxes := make(map[string]struct{})
	for _, definition := range definitions {
		if definition.Kind == string(deployment.DefinitionKindSandbox) {
			sandboxes[definition.DeclaredID] = struct{}{}
		}
	}
	plans := make([]scheduleReconciliation, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Kind != string(deployment.DefinitionKindTask) {
			continue
		}
		manifest, err := deployment.ParseTaskManifest(
			definition.ManifestVersion,
			definition.Manifest,
			definition.ManifestDigest,
		)
		if err != nil {
			return fmt.Errorf("parse task %q for schedule reconciliation: %w", definition.DeclaredID, err)
		}
		if manifest.Schedule == nil {
			continue
		}
		plan, err := prepareScheduleReconciliation(
			definition, *manifest.Schedule, sandboxes, effectiveFrom,
		)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	sort.Slice(plans, func(i, j int) bool {
		return plans[i].definition.DeclaredID < plans[j].definition.DeclaredID
	})
	scheduledIDs := make([]string, 0, len(plans))
	for i := range plans {
		plan := &plans[i]
		scheduledIDs = append(scheduledIDs, plan.definition.DeclaredID)
		record, err := store.ReconcileSchedule(ctx, db.ReconcileScheduleParams{
			ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID:          target.EnvironmentID,
			TaskDeclaredID:         plan.definition.DeclaredID,
			DeploymentDefinitionID: plan.definition.ID,
			DeploymentID:           target.ID,
			CronPattern:            plan.manifest.Cron,
			Timezone:               plan.manifest.Timezone,
			CronSemanticsVersion:   schedule.CronSemanticsVersion,
			State:                  "active",
			EffectiveFrom:          pgvalue.Timestamptz(effectiveFrom),
			NextFireAt:             pgvalue.Timestamptz(plan.nextFireAt),
		})
		if err != nil {
			return fmt.Errorf("reconcile schedule %q: %w", plan.definition.DeclaredID, err)
		}
		plan.record = record
	}
	if err := store.ArchiveOmittedSchedules(
		ctx,
		db.ArchiveOmittedSchedulesParams{
			EffectiveFrom:   pgvalue.Timestamptz(effectiveFrom),
			EnvironmentID:   target.EnvironmentID,
			TaskDeclaredIds: scheduledIDs,
		},
	); err != nil {
		return fmt.Errorf("archive omitted schedules: %w", err)
	}
	secretIDs := make(map[string]pgtype.UUID)
	for _, plan := range plans {
		for _, placement := range plan.placements {
			secretIDs[placement.Name] = pgtype.UUID{}
		}
	}
	secretNames := make([]string, 0, len(secretIDs))
	for name := range secretIDs {
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)
	if len(secretNames) > 0 {
		secretRecords, err := store.LockActiveSecretsByNameForWorkspaceCreate(
			ctx,
			db.LockActiveSecretsByNameForWorkspaceCreateParams{
				EnvironmentID: target.EnvironmentID,
				Names:         secretNames,
			},
		)
		if err != nil {
			return fmt.Errorf("lock scheduled Workspace Secrets: %w", err)
		}
		for _, record := range secretRecords {
			secretIDs[record.Name] = record.ID
		}
		for _, name := range secretNames {
			if !secretIDs[name].Valid {
				return badRequest(fmt.Errorf("scheduled Workspace Secret %q is unavailable", name))
			}
		}
	}
	for _, plan := range plans {
		if err := store.DeleteScheduleSecrets(ctx, db.DeleteScheduleSecretsParams{
			EnvironmentID: target.EnvironmentID,
			ScheduleID:    plan.record.ID,
		}); err != nil {
			return fmt.Errorf("replace schedule %q Secret selection: %w", plan.definition.DeclaredID, err)
		}
		for _, placement := range plan.placements {
			if _, err := store.CreateScheduleSecret(ctx, db.CreateScheduleSecretParams{
				ScheduleID:      plan.record.ID,
				EnvironmentID:   target.EnvironmentID,
				PlacementKind:   placement.Kind,
				PlacementTarget: placement.Target,
				SecretID:        secretIDs[placement.Name],
			}); err != nil {
				return fmt.Errorf("install schedule %q Secret selection: %w", plan.definition.DeclaredID, err)
			}
		}
	}
	return nil
}

func prepareScheduleReconciliation(
	definition db.DeploymentDefinition,
	manifest deployment.ScheduleManifest,
	sandboxes map[string]struct{},
	effectiveFrom time.Time,
) (scheduleReconciliation, error) {
	if err := schedule.ValidateCron(manifest.Cron); err != nil {
		return scheduleReconciliation{}, badRequest(fmt.Errorf("schedule %q cron: %w", definition.DeclaredID, err))
	}
	if err := schedule.ValidateTimezone(manifest.Timezone); err != nil {
		return scheduleReconciliation{}, badRequest(fmt.Errorf("schedule %q timezone: %w", definition.DeclaredID, err))
	}
	if _, ok := sandboxes[manifest.Workspace.SandboxDeclaredID]; !ok {
		return scheduleReconciliation{}, badRequest(fmt.Errorf(
			"schedule %q sandbox %q is absent from the deployment",
			definition.DeclaredID,
			manifest.Workspace.SandboxDeclaredID,
		))
	}
	placements, err := normalizeWorkspaceSecretPlacements(manifest.Workspace.Secrets)
	if err != nil {
		return scheduleReconciliation{}, badRequest(fmt.Errorf("schedule %q workspace secrets: %w", definition.DeclaredID, err))
	}
	next, err := schedule.NextCronTime(manifest.Cron, manifest.Timezone, effectiveFrom)
	if err != nil {
		return scheduleReconciliation{}, badRequest(fmt.Errorf("schedule %q next fire: %w", definition.DeclaredID, err))
	}
	return scheduleReconciliation{
		definition: definition,
		manifest:   manifest,
		placements: placements,
		nextFireAt: next,
	}, nil
}
