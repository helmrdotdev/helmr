package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type declarativeScheduleSyncStore interface {
	CreateSchedule(context.Context, db.CreateScheduleParams) (db.CreateScheduleRow, error)
	DeleteSchedule(context.Context, db.DeleteScheduleParams) (int64, error)
	GetActiveProjectGitHubRepositoryByFullName(context.Context, db.GetActiveProjectGitHubRepositoryByFullNameParams) (db.GetActiveProjectGitHubRepositoryByFullNameRow, error)
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
	Payload     json.RawMessage
	Workspace   api.ScheduleWorkspace
	Active      bool
}

func promoteDeploymentAndSyncSchedules(ctx context.Context, store interface {
	PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
}, params db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error) {
	row, err := store.PromoteDeployment(ctx, params)
	if err != nil {
		return db.PromoteDeploymentRow{}, err
	}
	syncStore, ok := store.(declarativeScheduleSyncStore)
	if !ok {
		return row, nil
	}
	if err := syncDeclarativeSchedulesForDeployment(ctx, syncStore, params.OrgID, params.ProjectID, params.EnvironmentID, params.DeploymentID); err != nil {
		return db.PromoteDeploymentRow{}, err
	}
	return row, nil
}

func syncDeclarativeSchedulesForDeployment(ctx context.Context, store declarativeScheduleSyncStore, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentID pgtype.UUID) error {
	desired, err := deploymentDeclarativeScheduleSpecs(ctx, store, orgID, projectID, environmentID, deploymentID)
	if err != nil {
		return err
	}
	currentRows, err := store.ListDeclarativeScheduleSummariesForEnvironment(ctx, db.ListDeclarativeScheduleSummariesForEnvironmentParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		return err
	}
	current := make(map[string]db.ListDeclarativeScheduleSummariesForEnvironmentRow, len(currentRows))
	for _, row := range currentRows {
		current[row.DedupKey] = row
	}
	seen := make(map[string]struct{}, len(desired))
	for _, spec := range desired {
		seen[spec.DedupKey] = struct{}{}
		if _, err := store.GetActiveProjectGitHubRepositoryByFullName(ctx, db.GetActiveProjectGitHubRepositoryByFullNameParams{
			OrgID:     orgID,
			ProjectID: projectID,
			FullName:  spec.Workspace.Repository,
		}); errors.Is(err, pgx.ErrNoRows) {
			return relabelGitHubSourceError(ghapp.InvalidSourceError{Err: fmt.Errorf("github repository %q is not enabled for this project workspace", spec.Workspace.Repository)}, "schedule workspace")
		} else if err != nil {
			return fmt.Errorf("authorize schedule workspace repository: %w", err)
		}
		next, err := schedule.NextCronTime(spec.Cron, spec.Timezone, time.Now())
		if err != nil {
			return err
		}
		workspaceJSON, err := json.Marshal(spec.Workspace)
		if err != nil {
			return err
		}
		runOptionsJSON, err := json.Marshal(api.CreateRunOptions{DeploymentID: ids.MustFromPG(deploymentID).String()})
		if err != nil {
			return err
		}
		secretBindingsJSON := json.RawMessage(`{}`)
		var nextScheduledAt pgtype.Timestamptz
		var nextDueAt pgtype.Timestamptz
		if spec.Active {
			nextScheduledAt = pgTimeToPG(next)
		}
		if row, ok := current[spec.DedupKey]; ok {
			if declarativeScheduleCurrent(row, spec, workspaceJSON, runOptionsJSON, secretBindingsJSON) {
				continue
			}
			if _, err := store.UpdateSchedule(ctx, db.UpdateScheduleParams{
				TaskID:               spec.TaskID,
				DedupKey:             spec.DedupKey,
				ExternalID:           pgtype.Text{String: spec.ScheduleKey, Valid: true},
				GeneratorType:        db.TaskScheduleGeneratorTypeCron,
				GeneratorExpression:  spec.Cron,
				GeneratorDescription: "",
				Timezone:             spec.Timezone,
				Payload:              spec.Payload,
				SecretBindings:       secretBindingsJSON,
				Workspace:            workspaceJSON,
				RunOptions:           runOptionsJSON,
				Active:               spec.Active,
				OrgID:                orgID,
				ProjectID:            projectID,
				EnvironmentID:        environmentID,
				ScheduleID:           row.ScheduleID,
				NextScheduledAt:      nextScheduledAt,
				JitterSeconds:        int64(schedule.DefaultJitter / time.Second),
			}); err != nil {
				return err
			}
			continue
		}
		scheduleID := ids.New()
		instanceID := ids.New()
		if spec.Active {
			nextDueAt = pgTimeToPG(next.Add(schedule.Jitter(instanceID, schedule.DefaultJitter)))
		}
		if _, err := store.CreateSchedule(ctx, db.CreateScheduleParams{
			ScheduleID:           ids.ToPG(scheduleID),
			OrgID:                orgID,
			ProjectID:            projectID,
			ScheduleType:         db.TaskScheduleTypeDeclarative,
			TaskID:               spec.TaskID,
			DedupKey:             spec.DedupKey,
			ExternalID:           pgtype.Text{String: spec.ScheduleKey, Valid: true},
			GeneratorType:        db.TaskScheduleGeneratorTypeCron,
			GeneratorExpression:  spec.Cron,
			GeneratorDescription: "",
			Timezone:             spec.Timezone,
			Payload:              spec.Payload,
			SecretBindings:       secretBindingsJSON,
			Workspace:            workspaceJSON,
			RunOptions:           runOptionsJSON,
			Active:               spec.Active,
			InstanceID:           ids.ToPG(instanceID),
			EnvironmentID:        environmentID,
			NextScheduledAt:      nextScheduledAt,
			NextDueAt:            nextDueAt,
		}); err != nil {
			return err
		}
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
			return err
		}
	}
	return nil
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
	payload := item.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return declarativeScheduleSpec{}, errors.New("schedule payload must be valid JSON")
	}
	workspace, err := ghapp.NormalizeSource(api.GitHubSource{
		Repository: item.Workspace.Repository,
		Ref:        item.Workspace.Ref,
		SHA:        item.Workspace.SHA,
		Subpath:    item.Workspace.Subpath,
	})
	if err != nil {
		return declarativeScheduleSpec{}, relabelGitHubSourceError(err, "schedule workspace")
	}
	active := true
	if item.Active != nil {
		active = *item.Active
	}
	return declarativeScheduleSpec{
		TaskID:      taskID,
		ScheduleKey: fmt.Sprintf("%s.%s", taskID, key),
		DedupKey:    declarativeScheduleDedupKey(orgID, projectID, environmentID, taskID, key),
		Cron:        cronExpression,
		Timezone:    timezone,
		Payload:     append(json.RawMessage(nil), payload...),
		Workspace: api.ScheduleWorkspace{
			Repository: workspace.Repository,
			Ref:        workspace.Ref,
			SHA:        workspace.SHA,
			Subpath:    workspace.Subpath,
		},
		Active: active,
	}, nil
}

func declarativeScheduleDedupKey(orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, scheduleID string) string {
	parts := []string{
		uuidFromPG(orgID).String(),
		uuidFromPG(projectID).String(),
		uuidFromPG(environmentID).String(),
		taskID,
		scheduleID,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("decl-%x", sum[:12])
}

func declarativeScheduleCurrent(row db.ListDeclarativeScheduleSummariesForEnvironmentRow, spec declarativeScheduleSpec, workspaceJSON []byte, runOptionsJSON []byte, secretBindingsJSON []byte) bool {
	if row.TaskID != spec.TaskID ||
		row.DedupKey != spec.DedupKey ||
		!row.ExternalID.Valid ||
		row.ExternalID.String != spec.ScheduleKey ||
		row.GeneratorType != db.TaskScheduleGeneratorTypeCron ||
		row.GeneratorExpression != spec.Cron ||
		row.Timezone != spec.Timezone ||
		row.ScheduleActive != spec.Active ||
		row.InstanceActive != spec.Active {
		return false
	}
	return jsonSemanticallyEqual(row.Payload, spec.Payload) &&
		jsonSemanticallyEqual(row.Workspace, workspaceJSON) &&
		jsonSemanticallyEqual(row.RunOptions, runOptionsJSON) &&
		jsonSemanticallyEqual(row.SecretBindings, secretBindingsJSON)
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
