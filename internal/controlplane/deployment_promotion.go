package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
	"uuid"

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
	record     db.ReconcileSchedulesRow
}

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
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
		if err := work.q.PromoteDeployment(
			r.Context(),
			db.PromoteDeploymentParams{
				OrgID:         target.OrgID,
				ProjectID:     target.ProjectID,
				EnvironmentID: target.EnvironmentID,
				DeploymentID:  target.ID,
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
	writeJSON(w, http.StatusOK, deploymentResponse(record))
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
	for index := 1; index < len(plans); index++ {
		if plans[index-1].definition.DeclaredID == plans[index].definition.DeclaredID {
			return badRequest(fmt.Errorf(
				"duplicate scheduled task %q",
				plans[index].definition.DeclaredID,
			))
		}
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
	scheduledIDs := make([]string, 0, len(plans))
	if len(plans) > 0 {
		params := db.ReconcileSchedulesParams{
			Ids:                     make([]pgtype.UUID, len(plans)),
			TaskDeclaredIds:         make([]string, len(plans)),
			DeploymentDefinitionIds: make([]pgtype.UUID, len(plans)),
			DeploymentIds:           make([]pgtype.UUID, len(plans)),
			CronPatterns:            make([]string, len(plans)),
			Timezones:               make([]string, len(plans)),
			EffectiveFroms:          make([]pgtype.Timestamptz, len(plans)),
			NextFireAts:             make([]pgtype.Timestamptz, len(plans)),
			EnvironmentID:           target.EnvironmentID,
			CronSemanticsVersion:    schedule.CronSemanticsVersion,
		}
		for index := range plans {
			plan := &plans[index]
			params.Ids[index] = pgvalue.UUID(uuid.NewV7())
			params.TaskDeclaredIds[index] = plan.definition.DeclaredID
			params.DeploymentDefinitionIds[index] = plan.definition.ID
			params.DeploymentIds[index] = target.ID
			params.CronPatterns[index] = plan.manifest.Cron
			params.Timezones[index] = plan.manifest.Timezone
			params.EffectiveFroms[index] = pgvalue.Timestamptz(effectiveFrom)
			params.NextFireAts[index] = pgvalue.Timestamptz(plan.nextFireAt)
		}
		records, err := store.ReconcileSchedules(ctx, params)
		if err != nil {
			return fmt.Errorf("reconcile schedules: %w", err)
		}
		if len(records) != len(plans) {
			return fmt.Errorf("reconcile schedules: returned %d rows for %d inputs", len(records), len(plans))
		}
		for index := range records {
			if records[index].TaskDeclaredID != plans[index].definition.DeclaredID {
				return fmt.Errorf(
					"reconcile schedules: row %d is task %q, expected %q",
					index, records[index].TaskDeclaredID, plans[index].definition.DeclaredID,
				)
			}
			plans[index].record = records[index]
		}
	}
	for i := range plans {
		plan := &plans[i]
		scheduledIDs = append(scheduledIDs, plan.definition.DeclaredID)
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
	if len(plans) == 0 {
		return nil
	}
	deletion := db.DeleteScheduleSecretsForSchedulesParams{
		ScheduleIds:   make([]pgtype.UUID, len(plans)),
		EnvironmentID: target.EnvironmentID,
	}
	insertion := db.InsertScheduleSecretsParams{
		ScheduleIds:   deletion.ScheduleIds,
		EnvironmentID: target.EnvironmentID,
	}
	for index, plan := range plans {
		deletion.ScheduleIds[index] = plan.record.ID
		for _, placement := range plan.placements {
			insertion.PlacementScheduleIds = append(insertion.PlacementScheduleIds, plan.record.ID)
			insertion.PlacementKinds = append(insertion.PlacementKinds, placement.Kind)
			insertion.PlacementTargets = append(insertion.PlacementTargets, placement.Target)
			insertion.SecretIds = append(insertion.SecretIds, secretIDs[placement.Name])
		}
	}
	if err := store.DeleteScheduleSecretsForSchedules(ctx, deletion); err != nil {
		return fmt.Errorf("delete schedule Secret selections: %w", err)
	}
	inserted, err := store.InsertScheduleSecrets(ctx, insertion)
	if err != nil {
		return fmt.Errorf("insert schedule Secret selections: %w", err)
	}
	if inserted != int64(len(insertion.PlacementScheduleIds)) {
		return fmt.Errorf(
			"insert schedule Secret selections: installed %d of %d placements",
			inserted, len(insertion.PlacementScheduleIds),
		)
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
