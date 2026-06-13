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
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"
)

const TriggerIdempotencyKeyTTL = "30d"

var ErrTriggerSuperseded = errors.New("schedule trigger was superseded")

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

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

func TriggerIdempotencyKey(instanceID pgtype.UUID, generation int64, scheduledAt pgtype.Timestamptz) string {
	return fmt.Sprintf("schedule:%s:%d:%s", pgvalue.MustUUIDValue(instanceID), generation, scheduledAt.Time.UTC().Format(time.RFC3339Nano))
}

func RunRequestFromTriggerCandidate(row db.GetScheduleTriggerCandidateRow) (api.CreateRunRequest, error) {
	return RunRequestFromTriggerCandidateAt(row, time.Now().UTC())
}

func RunRequestFromTriggerCandidateAt(row db.GetScheduleTriggerCandidateRow, now time.Time) (api.CreateRunRequest, error) {
	payload, err := scheduledTaskPayload(row, now)
	if err != nil {
		return api.CreateRunRequest{}, err
	}
	return runRequestFromScheduleSnapshot(row.ProjectID, row.EnvironmentID, row.TaskID, payload, row.RunOptions)
}

func runRequestFromScheduleSnapshot(projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, payload []byte, runOptions []byte) (api.CreateRunRequest, error) {
	var options api.CreateRunOptions
	if len(runOptions) > 0 {
		if err := json.Unmarshal(runOptions, &options); err != nil {
			return api.CreateRunRequest{}, err
		}
	}
	return api.CreateRunRequest{
		ProjectID:     pgvalue.MustUUIDValue(projectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(environmentID).String(),
		TaskID:        taskID,
		Payload:       append(json.RawMessage(nil), payload...),
		Options:       options,
	}, nil
}

func scheduledTaskPayload(row db.GetScheduleTriggerCandidateRow, now time.Time) ([]byte, error) {
	if !row.ScheduleID.Valid {
		return nil, errors.New("schedule id is required")
	}
	if !row.NextFireAt.Valid {
		return nil, errors.New("scheduled timestamp is required")
	}
	payload := map[string]any{
		"timestamp":    row.NextFireAt.Time.UTC().Format(time.RFC3339Nano),
		"timezone":     api.NormalizeTimezone(row.Timezone),
		"scheduleId":   pgvalue.MustUUIDValue(row.ScheduleID).String(),
		"scheduleType": string(row.ScheduleType),
	}
	if row.LastFireAt.Valid {
		payload["lastTimestamp"] = row.LastFireAt.Time.UTC().Format(time.RFC3339Nano)
	}
	if row.ExternalID.Valid {
		payload["externalId"] = row.ExternalID.String
	}
	upcomingAnchor := row.NextFireAt.Time.UTC()
	if upcomingAnchor.Before(now.UTC()) {
		upcomingAnchor = now.UTC()
	}
	upcoming, err := upcomingCronTimes(row.Cron, row.Timezone, upcomingAnchor, 5)
	if err != nil {
		return nil, err
	}
	encodedUpcoming := make([]string, 0, len(upcoming))
	for _, at := range upcoming {
		encodedUpcoming = append(encodedUpcoming, at.UTC().Format(time.RFC3339Nano))
	}
	payload["upcoming"] = encodedUpcoming
	return json.Marshal(payload)
}

func upcomingCronTimes(expression string, timezone string, anchor time.Time, count int) ([]time.Time, error) {
	if count <= 0 {
		return nil, nil
	}
	loc, err := time.LoadLocation(api.NormalizeTimezone(timezone))
	if err != nil {
		return nil, fmt.Errorf("timezone must be an IANA timezone")
	}
	spec, err := cronParser.Parse(strings.TrimSpace(expression))
	if err != nil {
		return nil, fmt.Errorf("cron must be a valid 5-field expression: %w", err)
	}
	upcoming := make([]time.Time, 0, count)
	cursor := anchor.In(loc)
	for len(upcoming) < count {
		next := spec.Next(cursor).UTC()
		if next.IsZero() {
			break
		}
		upcoming = append(upcoming, next)
		cursor = next.In(loc)
	}
	return upcoming, nil
}
