package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

type runIdempotency struct {
	key       pgtype.Text
	expiresAt pgtype.Timestamptz
	options   []byte
}

func normalizeRunIdempotency(options api.CreateRunOptions) (runIdempotency, error) {
	rawKey := strings.TrimSpace(options.IdempotencyKey)
	if rawKey == "" {
		if strings.TrimSpace(options.IdempotencyKeyTTL) != "" || len(options.IdempotencyKeyOptions) > 0 {
			return runIdempotency{}, errors.New("idempotency_key is required when idempotency options are set")
		}
		return runIdempotency{options: []byte(`{}`)}, nil
	}
	if len(rawKey) > maxIdempotencyKeyLength {
		return runIdempotency{}, fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}

	key := canonicalIdempotencyKey(rawKey)
	ttl, err := parseIdempotencyKeyTTL(options.IdempotencyKeyTTL)
	if err != nil {
		return runIdempotency{}, err
	}
	if ttl <= 0 {
		return runIdempotency{}, errors.New("idempotency_key_ttl must be positive")
	}
	idempotencyOptions := []byte(`{}`)
	if len(options.IdempotencyKeyOptions) > 0 {
		if !json.Valid(options.IdempotencyKeyOptions) {
			return runIdempotency{}, errors.New("idempotency_key_options must be valid JSON")
		}
		idempotencyOptions = append([]byte(nil), options.IdempotencyKeyOptions...)
	}
	return runIdempotency{
		key: pgtype.Text{
			String: key,
			Valid:  true,
		},
		expiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(ttl),
			Valid: true,
		},
		options: idempotencyOptions,
	}, nil
}

func canonicalIdempotencyKey(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func runIdempotencyRequestHash(request api.CreateRunRequest, payload json.RawMessage, deploymentTask db.GetDeploymentTaskRow, maxDurationSeconds int32, lockedRetryPolicy []byte, metadata []byte, tags []string, scheduling runScheduling) (pgtype.Text, error) {
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("payload canonicalization failed: %w", err)
	}
	fingerprint := struct {
		TaskID     string          `json:"task_id"`
		Payload    json.RawMessage `json:"payload"`
		Metadata   json.RawMessage `json:"metadata"`
		Tags       []string        `json:"tags"`
		Deployment struct {
			ID                 string `json:"id"`
			TaskID             string `json:"task_id"`
			BundleDigest       string `json:"bundle_digest,omitempty"`
			FilePath           string `json:"file_path,omitempty"`
			ExportName         string `json:"export_name,omitempty"`
			SourceDigest       string `json:"source_digest,omitempty"`
			MaxDurationSeconds int32  `json:"max_duration_seconds"`
		} `json:"deployment"`
		Scheduling struct {
			QueueName      string `json:"queue_name"`
			ConcurrencyKey string `json:"concurrency_key,omitempty"`
			Priority       int32  `json:"priority,omitempty"`
			TTL            string `json:"ttl,omitempty"`
		} `json:"options"`
		RetryPolicy json.RawMessage `json:"retry_policy"`
	}{
		TaskID:      request.TaskID,
		Payload:     canonicalPayload,
		Metadata:    json.RawMessage(metadata),
		Tags:        append([]string(nil), tags...),
		RetryPolicy: json.RawMessage(lockedRetryPolicy),
	}
	fingerprint.Deployment.ID = ids.MustFromPG(deploymentTask.DeploymentID).String()
	fingerprint.Deployment.TaskID = ids.MustFromPG(deploymentTask.ID).String()
	fingerprint.Deployment.BundleDigest = strings.TrimSpace(deploymentTask.BundleDigest)
	fingerprint.Deployment.FilePath = strings.TrimSpace(deploymentTask.FilePath)
	fingerprint.Deployment.ExportName = strings.TrimSpace(deploymentTask.ExportName)
	fingerprint.Deployment.SourceDigest = strings.TrimSpace(deploymentTask.DeploymentSourceDigest)
	fingerprint.Deployment.MaxDurationSeconds = maxDurationSeconds
	fingerprint.Scheduling.QueueName = scheduling.queueName
	if scheduling.concurrencyKey.Valid {
		fingerprint.Scheduling.ConcurrencyKey = scheduling.concurrencyKey.String
	}
	fingerprint.Scheduling.Priority = scheduling.priority
	fingerprint.Scheduling.TTL = scheduling.ttl

	body, err := json.Marshal(fingerprint)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("idempotency request fingerprint encode failed: %w", err)
	}
	digest := sha256.Sum256(body)
	return pgtype.Text{String: hex.EncodeToString(digest[:]), Valid: true}, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func parseIdempotencyKeyTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultIdempotencyKeyTTL, nil
	}
	return parsePositiveDuration(raw, "idempotency_key_ttl")
}

func parsePositiveDuration(raw string, label string) (time.Duration, error) {
	return api.ParsePositiveDuration(raw, label)
}

func (s *Server) existingIdempotentRun(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, key string, requestHash string, source runSource, allowTerminalClear bool) (runSummary, bool, error) {
	existing, err := s.db.GetScopedRunByIdempotencyKey(ctx, db.GetScopedRunByIdempotencyKeyParams{
		OrgID:          ids.ToPG(orgID),
		ProjectID:      projectID,
		EnvironmentID:  environmentID,
		TaskID:         taskID,
		IdempotencyKey: pgtype.Text{String: key, Valid: true},
	})
	if isNoRows(err) {
		return runSummary{}, false, nil
	}
	if err != nil {
		return runSummary{}, false, err
	}
	expired := existing.IdempotencyKeyExpiresAt.Valid && !time.Now().Before(existing.IdempotencyKeyExpiresAt.Time)
	if allowTerminalClear && (existing.Status == db.RunStatusFailed || existing.Status == db.RunStatusExpired || (expired && isTerminalRunStatus(existing.Status))) {
		if err := s.db.ClearRunIdempotencyKey(ctx, db.ClearRunIdempotencyKeyParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            existing.ID,
		}); err != nil {
			return runSummary{}, false, err
		}
		return runSummary{}, false, nil
	}
	if source.scheduleInstanceID.Valid && !idempotentRunMatchesScheduleSource(existing, source) {
		return runSummary{}, false, errIdempotencyKeyConflict
	}
	if existing.IdempotencyRequestHash.Valid && existing.IdempotencyRequestHash.String != requestHash && !source.scheduleInstanceID.Valid {
		return runSummary{}, false, errIdempotencyKeyConflict
	}
	return idempotentRunSummary(existing), true, nil
}

func idempotentRunMatchesScheduleSource(run db.GetScopedRunByIdempotencyKeyRow, source runSource) bool {
	return run.ScheduleID == source.scheduleID &&
		run.ScheduleInstanceID == source.scheduleInstanceID &&
		run.ScheduledAt.Valid == source.scheduledAt.Valid &&
		(!run.ScheduledAt.Valid || run.ScheduledAt.Time.UTC().Equal(source.scheduledAt.Time.UTC()))
}
