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
	var workspace api.ScheduleWorkspace
	if err := json.Unmarshal(row.Workspace, &workspace); err != nil {
		return api.CreateRunRequest{}, err
	}
	var options api.CreateRunOptions
	if len(row.RunOptions) > 0 {
		if err := json.Unmarshal(row.RunOptions, &options); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	var secrets api.SecretBindings
	if len(row.SecretBindings) > 0 {
		if err := json.Unmarshal(row.SecretBindings, &secrets); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	return api.CreateRunRequest{
		ProjectID:     ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(row.EnvironmentID).String(),
		TaskID:        row.TaskID,
		Secrets:       secrets,
		Payload:       append(json.RawMessage(nil), row.Payload...),
		Workspace: api.RunWorkspace{
			Repository: workspace.Repository,
			Ref:        workspace.Ref,
			SHA:        workspace.SHA,
			Subpath:    workspace.Subpath,
		},
		Options: options,
	}, nil
}
