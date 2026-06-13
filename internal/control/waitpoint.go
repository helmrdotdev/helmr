package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
	"github.com/jackc/pgx/v5/pgtype"
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
		writeError(w, badRequest(fmt.Errorf("invalid worker waitpoint request JSON: %w", err)))
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
	leaseRow, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
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
	requestJSON := request.Request
	if len(requestJSON) == 0 {
		requestJSON = []byte(`{}`)
	}
	if !json.Valid(requestJSON) {
		writeError(w, badRequest(errors.New("request must be valid JSON")))
		return
	}
	kind, displayText, err := waitpointRequestFields(request.Kind, requestJSON, request.DisplayText)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	timeout, err := waitpointTimeout(kind, request.TimeoutSeconds)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	policy, err := s.resolveWaitpointPolicy(r.Context(), leaseIDs.orgID, leaseRow.ProjectID, leaseRow.EnvironmentID, request.Policy)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	policyName := pgtype.Text{}
	var policySnapshot []byte
	if policy != nil {
		snapshot, err := policy.snapshot()
		if err != nil {
			writeError(w, errors.New("encode waitpoint policy"))
			return
		}
		policyName = pgvalue.Text(policy.Name)
		policySnapshot = snapshot
	}
	runWaitID := ids.New()
	waitpointID := ids.New()
	if linkedWaitpointID, ok, err := waitpointRequestLinkedID(kind, requestJSON); err != nil {
		writeError(w, badRequest(err))
		return
	} else if ok {
		waitpointID = linkedWaitpointID
	}
	checkpointID := ids.New()
	waitpoint, err := s.db.CreateWaitpointForExecution(r.Context(), db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		CheckpointID:     ids.ToPG(checkpointID),
		CheckpointReason: checkpointReason(kind),
		RunWaitID:        ids.ToPG(runWaitID),
		ID:               ids.ToPG(waitpointID),
		CorrelationID:    request.CorrelationID,
		Kind:             kind,
		Request:          requestJSON,
		DisplayText:      displayText,
		TimeoutSeconds:   timeout,
		PolicyName:       policyName,
		PolicySnapshot:   policySnapshot,
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
	writeJSON(w, http.StatusOK, api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    ids.MustFromPG(waitpoint.RunWaitID).String(),
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) createWaitpoint(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.CreateWaitpointRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid waitpoint request JSON: %w", err)))
		return
	}
	requestJSON := request.Request
	if len(requestJSON) == 0 {
		requestJSON = []byte(`{}`)
	}
	if !json.Valid(requestJSON) {
		writeError(w, badRequest(errors.New("request must be valid JSON")))
		return
	}
	if request.ExpiresAt.IsZero() || !request.ExpiresAt.After(time.Now().UTC()) {
		writeError(w, badRequest(errors.New("expires_at must be in the future")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	idempotencyKeyHash := pgtype.Text{}
	idempotencyKeyExpiresAt := pgtype.Timestamptz{}
	if idempotencyKey != "" {
		idempotencyKeyHash = pgtype.Text{String: waitpointCreationRequestHash(requestJSON, request.DisplayText, request.ExpiresAt), Valid: true}
		expiresAt, err := waitpointTokenExpiry(time.Now().UTC(), request.IdempotencyKeyExpiresAt, request.IdempotencyKeyTTLSeconds, defaultIdempotencyKeyTTL, "idempotency_key_expires")
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		idempotencyKeyExpiresAt = pgvalue.Timestamptz(expiresAt)
	}
	waitpoint, err := s.db.CreateHumanWaitpoint(r.Context(), db.CreateHumanWaitpointParams{
		ID:                      ids.ToPG(ids.New()),
		OrgID:                   ids.ToPG(actor.OrgID),
		ProjectID:               projectID,
		EnvironmentID:           environmentID,
		Request:                 requestJSON,
		DisplayText:             strings.TrimSpace(request.DisplayText),
		ExpiresAt:               pgvalue.Timestamptz(request.ExpiresAt.UTC()),
		IdempotencyKey:          pgvalue.Text(idempotencyKey),
		IdempotencyRequestHash:  idempotencyKeyHash,
		IdempotencyKeyExpiresAt: idempotencyKeyExpiresAt,
		IdempotencyKeyOptions:   []byte(`{}`),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("idempotency key reused with a different request")))
		return
	}
	if err != nil {
		s.log.Error("create waitpoint failed", "error", err)
		writeError(w, errors.New("create waitpoint"))
		return
	}
	writeJSON(w, http.StatusCreated, waitpointResponseFromCreate(waitpoint))
}

func (s *Server) respondWaitpoint(w http.ResponseWriter, r *http.Request) {
	var request api.RespondWaitpointRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid waitpoint response JSON: %w", err)))
		return
	}
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	waitpointID, err := ids.Parse(chi.URLParam(r, "waitpointID"))
	if err != nil {
		writeError(w, badRequest(errors.New("waitpointID must be a UUID")))
		return
	}
	actor := actorFromContext(r.Context())
	waitpoint, err := s.db.GetWaitpointForRespond(r.Context(), db.GetWaitpointForRespondParams{
		OrgID:       ids.ToPG(actor.OrgID),
		WaitpointID: ids.ToPG(waitpointID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("pending waitpoint not found")))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint before resolving failed", "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, errors.New("resolve waitpoint"))
		return
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(waitpoint.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(waitpoint.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, waitpoint.ProjectID, waitpoint.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("pending waitpoint not found")))
			return
		}
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	responseKey, principal, err := waitpointActorResponseIdentity(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	expectedKind := db.WaitpointKindHuman
	response, err := waitpointResponsePayload(expectedKind, principal, request.Value, request.Metadata, time.Now().UTC())
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	outcome, err := s.resolveWaitpointRecord(r.Context(), waitpointResolution{
		OrgID:           actor.OrgID,
		WaitpointID:     waitpointID,
		ResponseKey:     responseKey,
		Principal:       principal,
		ExternalSubject: request.ExternalSubject,
		ExpectedKind:    expectedKind,
		ResolutionKind:  response.ResolutionKind,
		OutputJSON:      response.Output,
		ResolutionJSON:  response.Resolution,
		EventPayload:    response.EventPayload,
		Metadata:        response.Metadata,
	})
	if err != nil {
		if isNoRows(err) {
			writeError(w, conflict(errors.New("waitpoint cannot be resolved")))
			return
		}
		s.log.Error("resolve waitpoint failed", "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, errors.New("resolve waitpoint"))
		return
	}
	if !outcome.Resumed {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type waitpointResolution struct {
	OrgID           uuid.UUID
	WaitpointID     uuid.UUID
	ResponseKey     string
	Principal       string
	ExternalSubject string
	ExpectedKind    db.WaitpointKind
	ResolutionKind  string
	OutputJSON      []byte
	ResolutionJSON  []byte
	EventPayload    map[string]any
	Metadata        []byte
}

type waitpointResolveOutcome struct {
	Resumed bool
}

func waitpointResponseFromCreate(row db.CreateHumanWaitpointRow) api.WaitpointResponse {
	expiresAt := pgvalue.Time(row.ExpiresAt)
	var expiresAtPtr *time.Time
	if row.ExpiresAt.Valid {
		expiresAtPtr = &expiresAt
	}
	return api.WaitpointResponse{
		ID:            ids.MustFromPG(row.ID).String(),
		ProjectID:     ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(row.EnvironmentID).String(),
		Kind:          string(row.Kind),
		Status:        string(row.Status),
		Request:       row.Request,
		DisplayText:   row.DisplayText,
		ExpiresAt:     expiresAtPtr,
		CreatedAt:     pgvalue.Time(row.CreatedAt),
	}
}

func waitpointCreationRequestHash(request json.RawMessage, displayText string, expiresAt time.Time) string {
	payload, _ := json.Marshal(map[string]any{
		"kind":         "human",
		"request":      json.RawMessage(request),
		"display_text": strings.TrimSpace(displayText),
		"expires_at":   expiresAt.UTC().Format(time.RFC3339Nano),
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (s *Server) resolveWaitpointRecord(ctx context.Context, resolution waitpointResolution) (waitpointResolveOutcome, error) {
	eventPayload := resolution.EventPayload
	if eventPayload == nil {
		eventPayload = map[string]any{}
	}
	waitpointID := resolution.WaitpointID
	eventPayload["waitpoint_id"] = waitpointID.String()
	eventJSON, err := json.Marshal(eventPayload)
	if err != nil {
		return waitpointResolveOutcome{}, fmt.Errorf("encode waitpoint resolved event: %w", err)
	}
	recordParams := db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          resolution.ResponseKey,
		RequestHash:          waitpointResponseRequestHash(resolution.OutputJSON, resolution.ExternalSubject, resolution.Metadata),
		Action:               "respond",
		ResolutionKind:       pgtype.Text{String: resolution.ResolutionKind, Valid: true},
		Resolution:           resolution.ResolutionJSON,
		EventPayload:         eventJSON,
		CompletedByPrincipal: pgtype.Text{String: resolution.Principal, Valid: true},
		CompletedVia:         pgtype.Text{String: "authenticated_api", Valid: true},
		ExternalSubject:      pgvalue.Text(resolution.ExternalSubject),
		Metadata:             resolution.Metadata,
		OrgID:                ids.ToPG(resolution.OrgID),
		WaitpointID:          ids.ToPG(waitpointID),
		Kind:                 resolution.ExpectedKind,
	}
	resolveParams := db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: resolution.ResolutionKind, Valid: true},
		Output:         resolution.OutputJSON,
		Resolution:     resolution.ResolutionJSON,
		OrgID:          ids.ToPG(resolution.OrgID),
		ID:             ids.ToPG(waitpointID),
		Kind:           resolution.ExpectedKind,
	}
	return s.recordAndResolveWaitpoint(ctx, recordParams, resolveParams)
}

func (s *Server) recordAndResolveWaitpoint(ctx context.Context, recordParams db.RecordWaitpointResponseParams, resolveParams db.ResolveWaitpointParams) (waitpointResolveOutcome, error) {
	if s.tx == nil {
		if store, ok := s.db.(interface {
			RecordAndResolveWaitpoint(context.Context, db.RecordWaitpointResponseParams, db.ResolveWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error)
		}); ok {
			resumed, err := store.RecordAndResolveWaitpoint(ctx, recordParams, resolveParams)
			if err != nil {
				return waitpointResolveOutcome{}, err
			}
			return waitpointResolveOutcome{Resumed: len(resumed) > 0}, nil
		}
		return waitpointResolveOutcome{}, errors.New("transactional waitpoint storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	if _, err := queries.RecordWaitpointResponse(ctx, recordParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{
		OrgID:       resolveParams.OrgID,
		WaitpointID: resolveParams.ID,
	})
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return waitpointResolveOutcome{}, err
	}
	return waitpointResolveOutcome{Resumed: len(resumed) > 0}, nil
}

func waitpointActorResponseIdentity(actor auth.Actor) (string, string, error) {
	responseKey, err := auth.ActorPrincipal(actor)
	if err != nil {
		return "", "", err
	}
	switch actor.Kind {
	case auth.ActorKindSession:
		return responseKey, actor.UserID.String(), nil
	case auth.ActorKindAPIKey:
		return responseKey, responseKey, nil
	default:
		return "", "", errors.New("supported actor identity is required")
	}
}

func waitpointRequestFields(kind api.WorkerWaitpointKind, request json.RawMessage, displayText string) (db.WaitpointKind, string, error) {
	displayText = strings.TrimSpace(displayText)
	switch kind {
	case api.WorkerWaitpointKindHuman:
		return db.WaitpointKindHuman, displayText, nil
	case api.WorkerWaitpointKindDelay:
		return db.WaitpointKindDelay, displayText, nil
	default:
		return "", "", fmt.Errorf("unsupported waitpoint kind %q", kind)
	}
}

func waitpointRequestLinkedID(kind db.WaitpointKind, request json.RawMessage) (uuid.UUID, bool, error) {
	if kind != db.WaitpointKindHuman {
		return uuid.Nil, false, nil
	}
	trimmed := bytes.TrimSpace(request)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return uuid.Nil, false, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return uuid.Nil, false, err
	}
	raw, ok, err := optionalStringField(payload, "waitpoint_id")
	if err != nil {
		return uuid.Nil, false, err
	}
	if !ok {
		raw, _, err = optionalStringField(payload, "waitpointId")
		if err != nil {
			return uuid.Nil, false, err
		}
	}
	if raw == "" {
		return uuid.Nil, false, nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return uuid.Nil, false, errors.New("request.waitpoint_id must be a UUID")
	}
	return id, true, nil
}

func optionalStringField(payload map[string]json.RawMessage, name string) (string, bool, error) {
	rawJSON, ok := payload[name]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(rawJSON, &value); err != nil {
		return "", false, nil
	}
	return strings.TrimSpace(value), true, nil
}

func waitpointTimeout(kind db.WaitpointKind, timeoutSeconds *int32) (pgtype.Int4, error) {
	if timeoutSeconds == nil {
		if kind == db.WaitpointKindDelay {
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
	ID             pgtype.UUID
	RunWaitID      pgtype.UUID
	OrgID          pgtype.UUID
	ProjectID      pgtype.UUID
	EnvironmentID  pgtype.UUID
	RunID          pgtype.UUID
	SessionID      pgtype.UUID
	CheckpointID   pgtype.UUID
	CorrelationID  string
	Kind           db.WaitpointKind
	Request        []byte
	DisplayText    string
	TimeoutSeconds pgtype.Int4
	PolicyName     pgtype.Text
	PolicySnapshot []byte
	Status         db.RunWaitStatus
	ResolutionKind pgtype.Text
	Resolution     []byte
	CreatedAt      pgtype.Timestamptz
	RequestedAt    pgtype.Timestamptz
	ResolvedAt     pgtype.Timestamptz
}

func (s *Server) notifyPendingWaitpoint(ctx context.Context, view waitpointView) {
	if s.waitpoints == nil {
		return
	}
	s.waitpoints.NotifyPending(ctx, pendingWaitpoint(view))
}

func pendingWaitpoint(view waitpointView) waitpoint.Pending {
	return waitpoint.Pending{
		ID:             view.ID,
		RunWaitID:      view.RunWaitID,
		OrgID:          view.OrgID,
		RunID:          view.RunID,
		Kind:           view.Kind,
		DisplayText:    view.DisplayText,
		PolicySnapshot: view.PolicySnapshot,
		RequestedAt:    view.RequestedAt,
	}
}
