package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const sessionInputBodyLimit = int64(maxActorInputBytes + 8192)

func (s *Server) sendSessionInput(w http.ResponseWriter, r *http.Request) {
	request, err := decodeSessionInputRequest(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{code: "session_input_too_large", message: "session input request is too large"}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_session_input", message: err.Error()}))
		return
	}
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	if err := api.ValidateSendSessionInputRequest(request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	canonicalInput, err := canonicalJSON(request.Input)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_input", message: err.Error()}))
		return
	}
	if len(canonicalInput) > maxActorInputBytes {
		writeError(w, tooLarge(codedError{code: "session_input_too_large", message: "session input exceeds the size limit"}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsInputSend, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	actor, err := s.db.GetActor(r.Context(), db.GetActorParams{
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(sessionID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "session_not_found", message: "session not found"}))
		return
	}
	if err != nil {
		writeError(w, errors.New("resolve session input address"))
		return
	}
	actorID, err := pgvalue.UUIDValue(actor.ID)
	if err != nil {
		writeError(w, errors.New("resolve session identity"))
		return
	}
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		writeError(w, errors.New("resolve session environment"))
		return
	}
	record, err := s.appendActorInput(r.Context(), appendActorInputRequest{
		EnvironmentID:  environmentUUID,
		SessionID:      actorID,
		RecordID:       uuid.NewV7(),
		Data:           canonicalInput,
		SourceKind:     "external",
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeSessionInputAppendError(w, r, actor, err)
		return
	}
	response, err := projectSessionInput(record)
	if err != nil {
		writeError(w, errors.New("project session input"))
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func projectSessionInput(record db.SessionRecord) (api.SessionInput, error) {
	id := pgvalue.UUIDString(record.ID)
	if ids.Validate(id) != nil || record.Direction != "input" || record.Sequence <= 0 ||
		!record.CreatedAt.Valid || !record.SourceKind.Valid || len(record.Data) == 0 {
		return api.SessionInput{}, errors.New("session input projection authority is invalid")
	}
	source := api.SessionInputSource{Type: record.SourceKind.String}
	if record.SourceRunID.Valid {
		source.RunID = pgvalue.UUIDString(record.SourceRunID)
		if ids.Validate(source.RunID) != nil {
			return api.SessionInput{}, errors.New("session input source authority is invalid")
		}
	}
	return api.SessionInput{
		ID: id, Sequence: record.Sequence, Data: json.RawMessage(record.Data),
		Source: source, CreatedAt: record.CreatedAt.Time.UTC(),
	}, nil
}

func decodeSessionInputRequest(r *http.Request) (api.SendSessionInputRequest, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return api.SendSessionInputRequest{}, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return api.SendSessionInputRequest{}, err
	}
	var request api.SendSessionInputRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return api.SendSessionInputRequest{}, err
	}
	if len(request.Input) == 0 {
		return api.SendSessionInputRequest{}, errors.New("input is required")
	}
	return request, nil
}

func (s *Server) writeSessionInputAppendError(w http.ResponseWriter, r *http.Request, actor db.Session, err error) {
	var conflictError idempotency.ConflictError
	switch {
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier session input"}))
	case errors.Is(err, errActorInputTooLarge):
		writeError(w, tooLarge(codedError{code: "session_input_too_large", message: err.Error()}))
	case errors.Is(err, errActorSequenceExhausted):
		writeError(w, conflict(codedError{code: "session_sequence_exhausted", message: err.Error()}))
	case errors.Is(err, errActorInputUnavailable):
		current, readErr := s.db.GetActor(r.Context(), db.GetActorParams{
			EnvironmentID: actor.EnvironmentID,
			ID:            actor.ID,
		})
		if readErr == nil && current.State == "open" && current.NextInputSequence > maxActorSequence {
			writeError(w, conflict(codedError{code: "session_sequence_exhausted", message: errActorSequenceExhausted.Error()}))
			return
		}
		writeError(w, conflict(codedError{code: "session_not_open", message: "session does not accept new input"}))
	case errors.Is(err, errActorInputAppendConflict):
		writeError(w, conflict(codedError{code: "session_input_conflict", message: err.Error()}))
	default:
		writeError(w, fmt.Errorf("append session input: %w", err))
	}
}
