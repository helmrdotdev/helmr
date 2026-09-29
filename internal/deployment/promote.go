package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type scheduleReconciliation struct {
	definition db.DeploymentDefinition
	manifest   definition.ScheduleManifest
	placements []secretbinding.Placement
	nextFireAt time.Time
	record     db.ReconcileSchedulesRow
}

// Promote makes a Deployment the environment's current one. In one
// transaction it locks the environment row, reconciles the environment's
// schedules and their Secret bindings to the Deployment's scheduled Tasks,
// switches the current Deployment and records the promotion event; any
// rejection rolls back the whole set. It returns the promoted Deployment.
func Promote(ctx context.Context, txb db.TxBeginner, principal auth.Actor, scope auth.Scope, deploymentID uuid.UUID) (db.Deployment, error) {
	if err := authorizeDeploy(principal, scope); err != nil {
		return db.Deployment{}, err
	}
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return db.Deployment{}, err
	}
	effectiveFrom := time.Now().UTC()
	var promoted db.Deployment
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		target, err := q.LockDeploymentPromotionTarget(ctx, db.LockDeploymentPromotionTargetParams{
			OrgID:         pgvalue.UUID(principal.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			DeploymentID:  pgvalue.UUID(deploymentID),
		})
		if isNoRows(err) {
			return ErrNotDeployable
		}
		if err != nil {
			return fmt.Errorf("lock deployment promotion target: %w", err)
		}
		if err := reconcileSchedules(ctx, q, target, effectiveFrom); err != nil {
			return err
		}
		if err := q.PromoteDeployment(ctx, db.PromoteDeploymentParams{
			OrgID:         target.OrgID,
			ProjectID:     target.ProjectID,
			EnvironmentID: target.EnvironmentID,
			DeploymentID:  target.ID,
		}); err != nil {
			return fmt.Errorf("promote deployment: %w", err)
		}
		if err := appendLifecycleEvent(
			ctx, q, target, "deployment.promoted", "info", "control", "promoted", "Deployment promoted",
		); err != nil {
			return fmt.Errorf("record deployment promotion event: %w", err)
		}
		promoted = target
		return nil
	})
	if err != nil {
		return db.Deployment{}, err
	}
	return promoted, nil
}

func reconcileSchedules(
	ctx context.Context,
	q db.Querier,
	target db.Deployment,
	effectiveFrom time.Time,
) error {
	definitions, err := q.ListDeploymentDefinitionsForDeployment(
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
	for _, deploymentDefinition := range definitions {
		if deploymentDefinition.Kind == string(definition.KindSandbox) {
			sandboxes[deploymentDefinition.DeclaredID] = struct{}{}
		}
	}
	plans := make([]scheduleReconciliation, 0, len(definitions))
	for _, deploymentDefinition := range definitions {
		if deploymentDefinition.Kind != string(definition.KindTask) {
			continue
		}
		manifest, err := definition.ParseTaskManifest(
			deploymentDefinition.ManifestVersion,
			deploymentDefinition.Manifest,
			deploymentDefinition.ManifestDigest,
		)
		if err != nil {
			return fmt.Errorf("parse task %q for schedule reconciliation: %w", deploymentDefinition.DeclaredID, err)
		}
		if manifest.Schedule == nil {
			continue
		}
		plan, err := prepareScheduleReconciliation(
			deploymentDefinition, *manifest.Schedule, sandboxes, effectiveFrom,
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
			return invalidInput(fmt.Errorf(
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
		secretRecords, err := q.LockActiveSecretsByNameForComputerCreate(
			ctx,
			db.LockActiveSecretsByNameForComputerCreateParams{
				EnvironmentID: target.EnvironmentID,
				Names:         secretNames,
			},
		)
		if err != nil {
			return fmt.Errorf("lock scheduled Computer Secrets: %w", err)
		}
		for _, record := range secretRecords {
			secretIDs[record.Name] = record.ID
		}
		for _, name := range secretNames {
			if !secretIDs[name].Valid {
				return invalidInput(fmt.Errorf("scheduled Computer Secret %q is unavailable", name))
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
			CronSemanticsVersion:    definition.CronSemanticsVersion,
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
		records, err := q.ReconcileSchedules(ctx, params)
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
	if err := q.ArchiveOmittedSchedules(
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
			insertion.Modes = append(insertion.Modes, placement.Mode)
			origins := placement.AllowedOrigins
			if origins == nil {
				origins = []string{}
			}
			encoded, err := json.Marshal(origins)
			if err != nil {
				return err
			}
			insertion.OriginsJson = append(insertion.OriginsJson, string(encoded))
		}
	}
	if err := q.DeleteScheduleSecretsForSchedules(ctx, deletion); err != nil {
		return fmt.Errorf("delete schedule Secret selections: %w", err)
	}
	inserted, err := q.InsertScheduleSecrets(ctx, insertion)
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
	deploymentDefinition db.DeploymentDefinition,
	manifest definition.ScheduleManifest,
	sandboxes map[string]struct{},
	effectiveFrom time.Time,
) (scheduleReconciliation, error) {
	if err := definition.ValidateCron(manifest.Cron); err != nil {
		return scheduleReconciliation{}, invalidInput(fmt.Errorf("schedule %q cron: %w", deploymentDefinition.DeclaredID, err))
	}
	if err := definition.ValidateTimezone(manifest.Timezone); err != nil {
		return scheduleReconciliation{}, invalidInput(fmt.Errorf("schedule %q timezone: %w", deploymentDefinition.DeclaredID, err))
	}
	if _, ok := sandboxes[manifest.Computer.SandboxDeclaredID]; !ok {
		return scheduleReconciliation{}, invalidInput(fmt.Errorf(
			"schedule %q sandbox %q is absent from the deployment",
			deploymentDefinition.DeclaredID,
			manifest.Computer.SandboxDeclaredID,
		))
	}
	placements, err := secretbinding.NormalizedPlacements(manifest.Computer.Secrets)
	if err != nil {
		return scheduleReconciliation{}, invalidInput(fmt.Errorf("schedule %q computer secrets: %w", deploymentDefinition.DeclaredID, err))
	}
	next, err := definition.NextCronTime(manifest.Cron, manifest.Timezone, effectiveFrom)
	if err != nil {
		return scheduleReconciliation{}, invalidInput(fmt.Errorf("schedule %q next fire: %w", deploymentDefinition.DeclaredID, err))
	}
	return scheduleReconciliation{
		definition: deploymentDefinition,
		manifest:   manifest,
		placements: placements,
		nextFireAt: next,
	}, nil
}

func appendLifecycleEvent(ctx context.Context, q db.Querier, target db.Deployment, kind string, severity string, source string, status string, message string) error {
	payload, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return err
	}
	if err := telemetry.ValidateEvent(message, payload); err != nil {
		return err
	}
	_, err = q.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{
		OrgID:          target.OrgID,
		ProjectID:      target.ProjectID,
		EnvironmentID:  target.EnvironmentID,
		DeploymentID:   target.ID,
		Category:       "lifecycle",
		Severity:       severity,
		Source:         source,
		Kind:           kind,
		Message:        message,
		Payload:        payload,
		RedactionClass: "internal",
	})
	return err
}
