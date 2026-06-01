package schedule

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"
)

const FireIdempotencyKeyTTL = "30d"

var ErrFireSuperseded = errors.New("schedule fire was superseded")

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

type FireSnapshot struct {
	TaskID         string
	Payload        []byte
	SecretBindings []byte
	Workspace      []byte
	RunOptions     []byte
}

func NextCronTime(expression string, timezone string, anchor time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(api.NormalizeTimezone(timezone))
	if err != nil {
		return time.Time{}, fmt.Errorf("timezone must be an IANA timezone")
	}
	spec, err := cronParser.Parse(strings.TrimSpace(expression))
	if err != nil {
		return time.Time{}, fmt.Errorf("cron must be a valid 5-field expression: %w", err)
	}
	next := spec.Next(anchor.In(loc)).UTC()
	if next.IsZero() {
		return time.Time{}, errors.New("cron has no future occurrences")
	}
	return next, nil
}

func Jitter(id uuid.UUID, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(id.String()))
	n := binary.BigEndian.Uint64(sum[:8])
	return time.Duration(n % uint64(window))
}

func RetryDelay(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Duration(attempt*attempt) * time.Minute
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func DefaultDedupKey(taskID string, expression string) string {
	sum := sha256.Sum256([]byte(taskID + "\n" + expression))
	return fmt.Sprintf("sch-%x", sum[:12])
}

func FireIdempotencyKey(row db.ClaimDueScheduleFiresRow) string {
	scheduledAt := row.ScheduledAt.Time.UTC()
	return fmt.Sprintf("schedule:%s:%s", ids.MustFromPG(row.ScheduleInstanceID), scheduledAt.Format(time.RFC3339Nano))
}

func RunRequestFromFire(row db.ClaimDueScheduleFiresRow) (api.CreateRunRequest, error) {
	return runRequestFromScheduleSnapshot(row.ProjectID, row.EnvironmentID, row.TaskID, row.Payload, row.SecretBindings, row.Workspace, row.RunOptions)
}

func RunRequestFromInstance(row db.ClaimDueScheduleInstancesRow) (api.CreateRunRequest, error) {
	return runRequestFromScheduleSnapshot(row.ProjectID, row.EnvironmentID, row.TaskID, row.Payload, row.SecretBindings, row.Workspace, row.RunOptions)
}

func runRequestFromScheduleSnapshot(projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, payload []byte, secretBindings []byte, workspaceJSON []byte, runOptions []byte) (api.CreateRunRequest, error) {
	var workspace api.ScheduleWorkspace
	if err := json.Unmarshal(workspaceJSON, &workspace); err != nil {
		return api.CreateRunRequest{}, err
	}
	var options api.CreateRunOptions
	if len(runOptions) > 0 {
		if err := json.Unmarshal(runOptions, &options); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	var secrets api.SecretBindings
	if len(secretBindings) > 0 {
		if err := json.Unmarshal(secretBindings, &secrets); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	return api.CreateRunRequest{
		ProjectID:     ids.MustFromPG(projectID).String(),
		EnvironmentID: ids.MustFromPG(environmentID).String(),
		TaskID:        taskID,
		Secrets:       secrets,
		Payload:       append(json.RawMessage(nil), payload...),
		Workspace: api.RunWorkspace{
			Repository: workspace.Repository,
			Ref:        workspace.Ref,
			SHA:        workspace.SHA,
			Subpath:    workspace.Subpath,
		},
		Options: options,
	}, nil
}
