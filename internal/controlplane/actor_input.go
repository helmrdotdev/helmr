package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const actorInputSendBodyLimit = int64(maxActorInputBytes + 8192)

func (s *Server) sendActorInput(w http.ResponseWriter, r *http.Request) {
	request, err := decodeActorInputRequest(r)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, tooLarge(codedError{code: "actor_input_too_large", message: "Actor input request is too large"}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_actor_input", message: err.Error()}))
		return
	}
	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	if err := api.ValidateSendActorInputRequest(request); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	canonicalInput, err := canonicalJSON(request.Input)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_input", message: err.Error()}))
		return
	}
	if len(canonicalInput) > maxActorInputBytes {
		writeError(w, tooLarge(codedError{code: "actor_input_too_large", message: "Actor input exceeds the size limit"}))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionActorsInputSend, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	actor, err := s.resolveActorInputAddress(r, environmentID, actorDeclaredID, request)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "actor_not_found", message: "Actor not found"}))
		return
	}
	if err != nil {
		writeError(w, errors.New("resolve Actor input address"))
		return
	}
	actorID, err := pgvalue.UUIDValue(actor.ID)
	if err != nil {
		writeError(w, errors.New("resolve Actor identity"))
		return
	}
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		writeError(w, errors.New("resolve Actor environment"))
		return
	}
	record, err := s.appendActorInput(r.Context(), appendActorInputRequest{
		EnvironmentID:  environmentUUID,
		ActorID:        actorID,
		RecordID:       uuid.Must(uuid.NewV7()),
		Data:           canonicalInput,
		SourceKind:     "external",
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeActorInputAppendError(w, r, actor, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SendActorInputResponse{Sequence: record.Sequence})
}

func decodeActorInputRequest(r *http.Request) (api.SendActorInputRequest, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return api.SendActorInputRequest{}, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return api.SendActorInputRequest{}, err
	}
	var request api.SendActorInputRequest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return api.SendActorInputRequest{}, err
	}
	if len(request.Input) == 0 {
		return api.SendActorInputRequest{}, errors.New("input is required")
	}
	return request, nil
}

func (s *Server) resolveActorInputAddress(
	r *http.Request,
	environmentID pgtype.UUID,
	actorDeclaredID string,
	request api.SendActorInputRequest,
) (db.Actor, error) {
	address := actorReadAddress{key: request.ActorKey}
	if request.ActorID != "" {
		id, err := ids.Parse(request.ActorID)
		if err != nil {
			return db.Actor{}, err
		}
		address.id = pgvalue.UUID(id)
	}
	return resolveActorAddress(
		r.Context(), s.db, environmentID, actorDeclaredID,
		address,
	)
}

func (s *Server) writeActorInputAppendError(w http.ResponseWriter, r *http.Request, actor db.Actor, err error) {
	var conflictError idempotency.ConflictError
	switch {
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier Actor input"}))
	case errors.Is(err, errActorInputTooLarge):
		writeError(w, tooLarge(codedError{code: "actor_input_too_large", message: err.Error()}))
	case errors.Is(err, errActorSequenceExhausted):
		writeError(w, conflict(codedError{code: "actor_sequence_exhausted", message: err.Error()}))
	case errors.Is(err, errActorInputUnavailable):
		current, readErr := s.db.GetActor(r.Context(), db.GetActorParams{
			EnvironmentID: actor.EnvironmentID,
			ID:            actor.ID,
		})
		if readErr == nil && current.State == "open" && current.NextInputSequence > maxActorSequence {
			writeError(w, conflict(codedError{code: "actor_sequence_exhausted", message: errActorSequenceExhausted.Error()}))
			return
		}
		writeError(w, conflict(codedError{code: "actor_not_open", message: "Actor does not accept new input"}))
	case errors.Is(err, errActorInputAppendConflict):
		writeError(w, conflict(codedError{code: "actor_input_conflict", message: err.Error()}))
	default:
		writeError(w, fmt.Errorf("append Actor input: %w", err))
	}
}
