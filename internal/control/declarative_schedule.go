package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5/pgtype"
)

type declarativeScheduleSyncStore interface {
	CreateDeclarativeSchedule(context.Context, db.CreateDeclarativeScheduleParams) (db.CreateDeclarativeScheduleRow, error)
	DeleteSchedule(context.Context, db.DeleteScheduleParams) (int64, error)
	ListDeclarativeScheduleSummariesForEnvironment(context.Context, db.ListDeclarativeScheduleSummariesForEnvironmentParams) ([]db.ListDeclarativeScheduleSummariesForEnvironmentRow, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
	PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
	UpdateSchedule(context.Context, db.UpdateScheduleParams) (db.UpdateScheduleRow, error)
}

type declarativeScheduleSpec struct {
	TaskID      string
	ScheduleKey string
	DedupKey    string
	Cron        string
	Timezone    string
	Active      bool
}

func promoteDeploymentAndSyncSchedules(ctx context.Context, store interface {
	PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
}, params db.PromoteDeploymentParams) (db.PromoteDeploymentRow, []scheduleView, error) {
	syncStore, ok := store.(declarativeScheduleSyncStore)
	if ok {
		if err := validateDeclarativeSchedulesForDeployment(ctx, syncStore, params.OrgID, params.ProjectID, params.EnvironmentID, params.DeploymentID); err != nil {
			return db.PromoteDeploymentRow{}, nil, err
		}
	}
	row, err := store.PromoteDeployment(ctx, params)
	if err != nil {
		return db.PromoteDeploymentRow{}, nil, err
	}
	if !ok {
		return row, nil, nil
	}
	changed, err := syncDeclarativeSchedulesForDeployment(ctx, syncStore, params.OrgID, params.ProjectID, params.EnvironmentID, params.DeploymentID)
	if err != nil {
		return db.PromoteDeploymentRow{}, nil, err
	}
	return row, changed, nil
}

func validateDeclarativeSchedulesForDeployment(ctx context.Context, store declarativeScheduleSyncStore, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID) error {
	_, err := deploymentDeclarativeScheduleSpecs(ctx, store, orgID, projectID, environmentID, deploymentID)
	if err != nil {
		return err
	}
	return nil
}

func syncDeclarativeSchedulesForDeployment(ctx context.Context, store declarativeScheduleSyncStore, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID) ([]scheduleView, error) {
	desired, err := deploymentDeclarativeScheduleSpecs(ctx, store, orgID, projectID, environmentID, deploymentID)
	if err != nil {
		return nil, err
	}
	currentRows, err := store.ListDeclarativeScheduleSummariesForEnvironment(ctx, db.ListDeclarativeScheduleSummariesForEnvironmentParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		return nil, err
	}
	current := make(map[string]db.ListDeclarativeScheduleSummariesForEnvironmentRow, len(currentRows))
	for _, row := range currentRows {
		current[row.DedupKey] = row
	}
	changed := []scheduleView{}
	seen := make(map[string]struct{}, len(desired))
	for _, spec := range desired {
		seen[spec.DedupKey] = struct{}{}
		next, err := schedule.NextCronTime(spec.Cron, spec.Timezone, time.Now())
		if err != nil {
			return nil, err
		}
		runOptionsJSON, err := json.Marshal(api.CreateRunOptions{})
		if err != nil {
			return nil, err
		}
		nextFireAt := pgTimeToPG(next)
		if row, ok := current[spec.DedupKey]; ok {
			if declarativeScheduleCurrent(row, spec, runOptionsJSON) {
				continue
			}
			updated, err := store.UpdateSchedule(ctx, db.UpdateScheduleParams{
				TaskID:        spec.TaskID,
				ExternalID:    pgtype.Text{String: spec.ScheduleKey, Valid: true},
				Cron:          spec.Cron,
				Timezone:      spec.Timezone,
				RunOptions:    runOptionsJSON,
				Active:        spec.Active,
				OrgID:         orgID,
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				ScheduleID:    row.ScheduleID,
				NextFireAt:    nextFireAt,
			})
			if err != nil {
				return nil, err
			}
			changed = append(changed, updatedScheduleView(updated))
			continue
		}
		scheduleID := ids.New()
		instanceID := ids.New()
		created, err := store.CreateDeclarativeSchedule(ctx, db.CreateDeclarativeScheduleParams{
			ScheduleID:    ids.ToPG(scheduleID),
			OrgID:         orgID,
			ProjectID:     projectID,
			TaskID:        spec.TaskID,
			DedupKey:      spec.DedupKey,
			ExternalID:    pgtype.Text{String: spec.ScheduleKey, Valid: true},
			Cron:          spec.Cron,
			Timezone:      spec.Timezone,
			RunOptions:    runOptionsJSON,
			Active:        spec.Active,
			InstanceID:    ids.ToPG(instanceID),
			EnvironmentID: environmentID,
			NextFireAt:    nextFireAt,
		})
		if err != nil {
			return nil, err
		}
		changed = append(changed, createDeclarativeScheduleView(created))
	}
	for _, row := range currentRows {
		if _, ok := seen[row.DedupKey]; ok {
			continue
		}
		if _, err := store.DeleteSchedule(ctx, db.DeleteScheduleParams{
			OrgID:         orgID,
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ScheduleID:    row.ScheduleID,
		}); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func deploymentDeclarativeScheduleSpecs(ctx context.Context, store declarativeScheduleSyncStore, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID) ([]declarativeScheduleSpec, error) {
	tasks, err := store.ListDeploymentTasks(ctx, db.ListDeploymentTasksParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deploymentID,
	})
	if err != nil {
		return nil, err
	}
	specs := []declarativeScheduleSpec{}
	for _, task := range tasks {
		var schedules []api.WorkerDeploymentTaskSchedule
		if len(task.ScheduleDeclarations) > 0 {
			if err := json.Unmarshal(task.ScheduleDeclarations, &schedules); err != nil {
				return nil, fmt.Errorf("decode task %q schedule declarations: %w", task.TaskID, err)
			}
		}
		for _, item := range schedules {
			spec, err := normalizeDeclarativeScheduleSpec(orgID, projectID, environmentID, task.TaskID, item)
			if err != nil {
				return nil, err
			}
			specs = append(specs, spec)
		}
	}
	return specs, nil
}

func normalizeDeclarativeScheduleSpec(orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, item api.WorkerDeploymentTaskSchedule) (declarativeScheduleSpec, error) {
	taskID = strings.TrimSpace(taskID)
	if err := api.ValidateTaskID(taskID); err != nil {
		return declarativeScheduleSpec{}, err
	}
	key := strings.TrimSpace(item.ID)
	if key == "" {
		key = "default"
	}
	if err := api.ValidateScheduleID(key); err != nil {
		return declarativeScheduleSpec{}, err
	}
	cronExpression := strings.TrimSpace(item.Cron)
	timezone := api.NormalizeTimezone(item.Timezone)
	if _, err := schedule.NextCronTime(cronExpression, timezone, time.Now()); err != nil {
		return declarativeScheduleSpec{}, err
	}
	active := true
	if item.Active != nil {
		active = *item.Active
	}
	return declarativeScheduleSpec{
		TaskID:      taskID,
		ScheduleKey: fmt.Sprintf("%s.%s", taskID, key),
		DedupKey:    declarativeScheduleDedupKey(orgID, projectID, taskID, key),
		Cron:        cronExpression,
		Timezone:    timezone,
		Active:      active,
	}, nil
}

func declarativeScheduleDedupKey(orgID pgtype.UUID, projectID pgtype.UUID, taskID string, scheduleID string) string {
	parts := []string{
		uuidFromPG(orgID).String(),
		uuidFromPG(projectID).String(),
		taskID,
		scheduleID,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("decl-%x", sum[:12])
}

func declarativeScheduleCurrent(row db.ListDeclarativeScheduleSummariesForEnvironmentRow, spec declarativeScheduleSpec, runOptionsJSON []byte) bool {
	if row.TaskID != spec.TaskID ||
		row.DedupKey != spec.DedupKey ||
		!row.ExternalID.Valid ||
		row.ExternalID.String != spec.ScheduleKey ||
		row.Cron != spec.Cron ||
		row.Timezone != spec.Timezone ||
		!row.ScheduleActive ||
		row.InstanceActive != spec.Active {
		return false
	}
	return jsonSemanticallyEqual(row.RunOptions, runOptionsJSON)
}

func jsonSemanticallyEqual(a []byte, b []byte) bool {
	var av any
	var bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func uuidFromPG(value pgtype.UUID) uuid.UUID {
	return ids.MustFromPG(value)
}
