package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	waitMetadataJSONMaxBytes = 64 * 1024
	waitTagsMaxCount         = 32
	waitTagMaxBytes          = 128
)

func (s *Server) workerCreateWaitpoint(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerCreateWaitpointRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker waitpoint JSON: %w", err)))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker run lease belongs to another worker")))
		return
	}
	_, _, err = s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return
	}
	request.CorrelationID = strings.TrimSpace(request.CorrelationID)
	if request.CorrelationID == "" {
		writeError(w, badRequest(errors.New("correlation_id is required")))
		return
	}
	paramsJSON := request.Params
	if len(paramsJSON) == 0 {
		paramsJSON = []byte(`{}`)
	}
	if !json.Valid(paramsJSON) {
		writeError(w, badRequest(errors.New("params must be valid JSON")))
		return
	}
	metadataJSON := request.Metadata
	if len(metadataJSON) == 0 {
		metadataJSON = []byte(`{}`)
	}
	if err := validateWaitpointMetadata(metadataJSON); err != nil {
		writeError(w, badRequest(err))
		return
	}
	tags, err := normalizeWaitpointTags(request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	kind, err := waitpointRequestKind(request.Kind)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var inlineToken waitpointInlineToken
	if kind == db.WaitpointKindToken {
		var err error
		inlineToken, paramsJSON, err = extractInlineWaitpointToken(paramsJSON)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	if kind == db.WaitpointKindChannel {
		var err error
		paramsJSON, err = normalizeChannelWaitpointParams(paramsJSON)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	timeout, err := waitpointTimeout(kind, request.TimeoutSeconds)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.Ordinal < 0 {
		writeError(w, badRequest(errors.New("ordinal must be non-negative")))
		return
	}
	runSuspensionID := uuid.Must(uuid.NewV7())
	waitpointID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	waitpoint, err := s.db.CreateRunSuspensionForWaitpoint(r.Context(), db.CreateRunSuspensionForWaitpointParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		CheckpointID:     pgvalue.UUID(checkpointID),
		CheckpointReason: checkpointReason(kind),
		RunSuspensionID:  pgvalue.UUID(runSuspensionID),
		ID:               pgvalue.UUID(waitpointID),
		CorrelationID:    request.CorrelationID,
		Kind:             kind,
		Params:           paramsJSON,
		Metadata:         metadataJSON,
		Tags:             tags,
		TimeoutSeconds:   timeout,
		Ordinal:          request.Ordinal,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("create waitpoint failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("create waitpoint"))
		return
	}
	if inlineToken.ID != uuid.Nil {
		if err := s.attachInlineWaitpointToken(r.Context(), waitpoint, inlineToken); err != nil {
			s.log.Error("attach waitpoint token failed", "run_id", request.Lease.RunID, "waitpoint_id", pgvalue.MustUUIDValue(waitpoint.ID).String(), "token_id", inlineToken.ID.String(), "error", err)
			if cleanupErr := s.db.CancelOpeningRunSuspension(r.Context(), db.CancelOpeningRunSuspensionParams{
				OrgID:           pgvalue.UUID(leaseIDs.orgID),
				RunSuspensionID: waitpoint.RunSuspensionID,
				ErrorMessage:    "waitpoint token attach failed",
			}); cleanupErr != nil {
				s.log.Error("cancel opening waitpoint suspension failed", "run_id", request.Lease.RunID, "run_suspension_id", pgvalue.MustUUIDValue(waitpoint.RunSuspensionID).String(), "error", cleanupErr)
			}
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, workerCreateWaitpointResponse(request.Lease.RunID, waitpoint))
}

func (s *Server) attachInlineWaitpointToken(ctx context.Context, waitpoint db.CreateRunSuspensionForWaitpointRow, inlineToken waitpointInlineToken) error {
	if _, err := s.db.AttachWaitpointTokenToWaitpoint(ctx, db.AttachWaitpointTokenToWaitpointParams{
		OrgID:       waitpoint.OrgID,
		WaitpointID: waitpoint.ID,
		TokenID:     pgvalue.UUID(inlineToken.ID),
	}); isNoRows(err) {
		return conflict(errors.New("waitpoint token not found or cannot attach to this waitpoint"))
	} else if err != nil {
		return errors.New("attach waitpoint token")
	}
	return nil
}

func workerCreateWaitpointResponse(runID string, waitpoint db.CreateRunSuspensionForWaitpointRow) api.WorkerCreateWaitpointResponse {
	response := api.WorkerCreateWaitpointResponse{
		RunID:           runID,
		RunSuspensionID: pgvalue.MustUUIDValue(waitpoint.RunSuspensionID).String(),
		WaitpointID:     pgvalue.MustUUIDValue(waitpoint.ID).String(),
		CheckpointID:    pgvalue.MustUUIDValue(waitpoint.CheckpointID).String(),
	}
	if waitpoint.ResolutionKind.Valid {
		response.ResolutionKind = waitpoint.ResolutionKind.String
		response.Resolution = json.RawMessage(waitpoint.Resolution)
	}
	return response
}

type waitpointInlineToken struct {
	ID uuid.UUID
}

func extractInlineWaitpointToken(paramsJSON []byte) (waitpointInlineToken, []byte, error) {
	var payload struct {
		TokenID string `json:"token_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(paramsJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return waitpointInlineToken{}, nil, err
	}
	if strings.TrimSpace(payload.TokenID) == "" {
		return waitpointInlineToken{}, nil, errors.New("wait.forToken token_id is required")
	}
	tokenID, err := uuid.Parse(strings.TrimSpace(payload.TokenID))
	if err != nil {
		return waitpointInlineToken{}, nil, errors.New("wait.forToken token_id must be a UUID")
	}
	redacted, err := json.Marshal(map[string]string{"token_id": tokenID.String()})
	if err != nil {
		return waitpointInlineToken{}, nil, err
	}
	return waitpointInlineToken{ID: tokenID}, redacted, nil
}

type waitpointResolveOutcome struct {
	Resumed bool
	OrgID   pgtype.UUID
	RunIDs  []pgtype.UUID
}

func waitpointRequestKind(kind api.WorkerWaitpointKind) (db.WaitpointKind, error) {
	switch kind {
	case api.WorkerWaitpointKindToken:
		return db.WaitpointKindToken, nil
	case api.WorkerWaitpointKindTimer:
		return db.WaitpointKindTimer, nil
	case api.WorkerWaitpointKindChannel:
		return db.WaitpointKindChannel, nil
	default:
		return "", fmt.Errorf("unsupported waitpoint kind %q", kind)
	}
}

func normalizeChannelWaitpointParams(paramsJSON []byte) ([]byte, error) {
	var payload struct {
		Channel       string `json:"channel"`
		AfterSequence *int64 `json:"after_sequence,omitempty"`
		CorrelationID string `json:"correlation_id,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(paramsJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	channel := strings.TrimSpace(payload.Channel)
	if err := validateChannelName(channel); err != nil {
		return nil, fmt.Errorf("channel waitpoint channel is invalid: %w", err)
	}
	afterSequence := int64(0)
	if payload.AfterSequence != nil {
		if *payload.AfterSequence < 0 {
			return nil, errors.New("channel waitpoint after_sequence must be non-negative")
		}
		afterSequence = *payload.AfterSequence
	}
	correlationID := strings.TrimSpace(payload.CorrelationID)
	normalized, err := json.Marshal(struct {
		Channel       string `json:"channel"`
		AfterSequence int64  `json:"after_sequence"`
		CorrelationID string `json:"correlation_id,omitempty"`
	}{
		Channel:       channel,
		AfterSequence: afterSequence,
		CorrelationID: correlationID,
	})
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func normalizeWaitpointTags(tags []string) ([]string, error) {
	if len(tags) > waitTagsMaxCount {
		return nil, fmt.Errorf("tags has %d entries, exceeds max %d", len(tags), waitTagsMaxCount)
	}
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, errors.New("tags must be non-empty")
		}
		if len([]byte(tag)) > waitTagMaxBytes {
			return nil, fmt.Errorf("tag is %d bytes, exceeds max %d", len([]byte(tag)), waitTagMaxBytes)
		}
		normalized = append(normalized, tag)
	}
	return normalized, nil
}

func validateWaitpointMetadata(metadata []byte) error {
	_, err := normalizeJSONMetadataObject(metadata, "metadata")
	return err
}

func normalizeJSONMetadataObject(metadata []byte, label string) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, metadata); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON: %w", label, err)
	}
	if compact.Len() > waitMetadataJSONMaxBytes {
		return nil, fmt.Errorf("%s is %d bytes, exceeds max %d", label, compact.Len(), waitMetadataJSONMaxBytes)
	}
	if !jsonObject(compact.Bytes()) {
		return nil, fmt.Errorf("%s must be a JSON object", label)
	}
	return compact.Bytes(), nil
}

func jsonObject(value []byte) bool {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false
	}
	return decoded != nil
}

func waitpointTimeout(kind db.WaitpointKind, timeoutSeconds *int32) (pgtype.Int4, error) {
	if timeoutSeconds == nil {
		if kind == db.WaitpointKindTimer {
			return pgtype.Int4{}, errors.New("timeout_seconds is required for delay waitpoints")
		}
		return pgtype.Int4{}, nil
	}
	if *timeoutSeconds <= 0 {
		return pgtype.Int4{}, errors.New("timeout_seconds must be positive")
	}
	return pgtype.Int4{Int32: *timeoutSeconds, Valid: true}, nil
}

func optionalPositiveInt32(value int64, field string) (*int32, error) {
	if value == 0 {
		return nil, nil
	}
	if value < 0 || value > math.MaxInt32 {
		return nil, fmt.Errorf("%s must be between 1 and %d", field, math.MaxInt32)
	}
	out := int32(value)
	return &out, nil
}

func optionalTrimmedString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func decodeOptionalJSON(r io.Reader, out any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type waitpointView struct {
	ID              pgtype.UUID
	RunSuspensionID pgtype.UUID
	OrgID           pgtype.UUID
	ProjectID       pgtype.UUID
	EnvironmentID   pgtype.UUID
	RunID           pgtype.UUID
	RunLeaseID      pgtype.UUID
	CheckpointID    pgtype.UUID
	CorrelationID   string
	Kind            db.WaitpointKind
	WaitpointStatus string
	Params          []byte
	Metadata        []byte
	Tags            []string
	TimeoutSeconds  pgtype.Int4
	Status          db.RunSuspensionStatus
	ResolutionKind  pgtype.Text
	Resolution      []byte
	CreatedAt       pgtype.Timestamptz
	WaitingAt       pgtype.Timestamptz
	ResolvedAt      pgtype.Timestamptz
}
