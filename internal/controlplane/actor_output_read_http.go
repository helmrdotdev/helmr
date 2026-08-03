package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errActorOutputCursorExpired = errors.New("Actor output cursor is older than the retained output")

const (
	actorOutputReadDefaultLimit = int32(50)
	actorOutputReadMaxLimit     = int32(100)
	maxActorOutputSequence      = int64(1<<53 - 1)
	maxActorOutputFrontier      = int64(1 << 53)
)

type actorOutputReadRequest struct {
	address actorReadAddress
	after   *int64
	limit   int32
}

type actorOutputReferenceError struct {
	err error
}

func (e actorOutputReferenceError) Error() string {
	return e.err.Error()
}

func (e actorOutputReferenceError) Unwrap() error {
	return e.err
}

func (s *Server) readActorOutputHTTP(w http.ResponseWriter, r *http.Request) {
	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	request, err := parseActorOutputReadRequest(r)
	if err != nil {
		var referenceError actorOutputReferenceError
		if errors.As(err, &referenceError) {
			writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_actor_output_read", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeActorOutputReadBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	scope, environmentID, err := s.actorReadScope(r, principal)
	if err != nil {
		s.writeActorOutputReadScopeError(w, err)
		return
	}
	if !principal.HasPermission(auth.PermissionActorsRead, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return
	}
	if s.db == nil {
		s.writeActorOutputReadAuthorityError(w)
		return
	}

	response, err := readActorOutputPage(
		r.Context(), s.db, environmentID, actorDeclaredID,
		request.address, request.after, request.limit,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, notFound(codedError{code: "actor_not_found", message: "Actor not found"}))
			return
		}
		if errors.Is(err, errActorOutputCursorExpired) {
			writeError(w, gone(codedError{
				code: "actor_output_cursor_expired", message: err.Error(),
			}))
			return
		}
		s.writeActorOutputReadAuthorityError(w)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func readActorOutputPage(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	actorDeclaredID string,
	address actorReadAddress,
	after *int64,
	limit int32,
) (api.ActorOutputPage, error) {
	var afterSequence int64
	afterPresent := after != nil
	if afterPresent {
		afterSequence = *after
	}
	rows, err := store.ReadPublicActorOutputPage(ctx, db.ReadPublicActorOutputPageParams{
		LimitCount: limit + 1, AfterPresent: afterPresent,
		AfterSequence: afterSequence, EnvironmentID: environmentID,
		ActorDeclaredID: actorDeclaredID,
		AddressID:       address.id,
		AddressKey:      pgvalue.Text(address.key),
	})
	if err != nil {
		return api.ActorOutputPage{}, err
	}
	if len(rows) == 0 {
		return api.ActorOutputPage{}, pgx.ErrNoRows
	}
	first := rows[0]
	if first.OutputRetentionFloor < 1 ||
		first.OutputRetentionFloor > maxActorOutputFrontier ||
		first.NextOutputSequence < first.OutputRetentionFloor ||
		first.NextOutputSequence > maxActorOutputFrontier ||
		first.EffectiveAfter < 0 ||
		first.EffectiveAfter > maxActorOutputSequence {
		return api.ActorOutputPage{}, errors.New("Actor output projection is invalid")
	}
	if afterPresent && afterSequence+1 < first.OutputRetentionFloor {
		return api.ActorOutputPage{}, errActorOutputCursorExpired
	}

	response := api.ActorOutputPage{
		Records:   make([]api.ActorOutputRecord, 0, min(len(rows), int(limit))),
		NextAfter: first.EffectiveAfter,
	}
	for _, row := range rows {
		if row.ActorID != first.ActorID ||
			row.OutputRetentionFloor != first.OutputRetentionFloor ||
			row.NextOutputSequence != first.NextOutputSequence ||
			row.EffectiveAfter != first.EffectiveAfter {
			return api.ActorOutputPage{}, errors.New("Actor output projection is inconsistent")
		}
		if !row.RecordID.Valid {
			if len(rows) != 1 {
				return api.ActorOutputPage{}, errors.New("Actor output empty projection is inconsistent")
			}
			break
		}
		record, err := projectActorOutputRecord(row)
		if err != nil {
			return api.ActorOutputPage{}, err
		}
		response.Records = append(response.Records, record)
	}
	response.HasMore = len(response.Records) > int(limit)
	if response.HasMore {
		response.Records = response.Records[:limit]
	}
	if len(response.Records) > 0 {
		response.NextAfter = response.Records[len(response.Records)-1].Sequence
	}
	return response, nil
}

func parseActorOutputReadRequest(r *http.Request) (actorOutputReadRequest, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return actorOutputReadRequest{}, errors.New("query string is malformed")
	}
	for name, entries := range values {
		switch name {
		case "actor_id", "actor_key", "after", "limit":
		default:
			return actorOutputReadRequest{}, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			err := fmt.Errorf("query parameter %q must appear exactly once with a non-empty value", name)
			if name == "actor_id" || name == "actor_key" {
				return actorOutputReadRequest{}, actorOutputReferenceError{err: err}
			}
			return actorOutputReadRequest{}, err
		}
	}
	rawID, hasID, err := singleNonEmptyQueryValue(values, "actor_id")
	if err != nil {
		return actorOutputReadRequest{}, actorOutputReferenceError{err: err}
	}
	key, hasKey, err := singleNonEmptyQueryValue(values, "actor_key")
	if err != nil {
		return actorOutputReadRequest{}, actorOutputReferenceError{err: err}
	}
	if hasID == hasKey {
		return actorOutputReadRequest{}, actorOutputReferenceError{
			err: errors.New("exactly one of actor_id or actor_key is required"),
		}
	}
	request := actorOutputReadRequest{limit: actorOutputReadDefaultLimit}
	if hasID {
		id, err := ids.Parse(rawID)
		if err != nil {
			return actorOutputReadRequest{}, actorOutputReferenceError{err: err}
		}
		request.address.id = pgvalue.UUID(id)
	} else {
		if err := api.ValidateActorKey(key); err != nil {
			return actorOutputReadRequest{}, actorOutputReferenceError{err: err}
		}
		request.address.key = key
	}
	if entries, ok := values["after"]; ok {
		value, err := parseActorOutputDecimal(entries[0], 0, maxActorOutputSequence, "after")
		if err != nil {
			return actorOutputReadRequest{}, err
		}
		request.after = &value
	}
	if entries, ok := values["limit"]; ok {
		value, err := parseActorOutputDecimal(entries[0], 1, int64(actorOutputReadMaxLimit), "limit")
		if err != nil {
			return actorOutputReadRequest{}, err
		}
		request.limit = int32(value)
	}
	return request, nil
}

func parseActorOutputDecimal(raw string, minimum, maximum int64, name string) (int64, error) {
	for _, value := range []byte(raw) {
		if value < '0' || value > '9' {
			return 0, fmt.Errorf("%s must be an integer in [%d,%d]", name, minimum, maximum)
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer in [%d,%d]", name, minimum, maximum)
	}
	return value, nil
}

func projectActorOutputRecord(row db.ReadPublicActorOutputPageRow) (api.ActorOutputRecord, error) {
	recordUUID := uuid.UUID(row.RecordID.Bytes)
	recordID := recordUUID.String()
	if err := ids.Validate(recordID); err != nil {
		return api.ActorOutputRecord{}, err
	}
	if row.Sequence <= row.EffectiveAfter ||
		row.Sequence >= row.NextOutputSequence ||
		row.Sequence > maxActorOutputSequence ||
		!json.Valid(row.Data) ||
		row.ContentType == "" ||
		!row.CreatedAt.Valid ||
		row.ProducerAttemptNumber < 1 {
		return api.ActorOutputRecord{}, errors.New("actor output record projection is invalid")
	}
	runID := pgvalue.UUIDString(row.RunID)
	if err := ids.Validate(runID); err != nil {
		return api.ActorOutputRecord{}, errors.New("actor output producer Run ID is invalid")
	}
	deploymentID := pgvalue.UUIDString(row.DeploymentID)
	if err := ids.Validate(deploymentID); err != nil {
		return api.ActorOutputRecord{}, errors.New("actor output Deployment ID is invalid")
	}
	return api.ActorOutputRecord{
		ID:          recordID,
		Sequence:    row.Sequence,
		Data:        append(json.RawMessage(nil), row.Data...),
		ContentType: row.ContentType,
		CreatedAt:   row.CreatedAt.Time.UTC(),
		Provenance: api.ActorOutputProvenance{
			RunID:         runID,
			AttemptNumber: row.ProducerAttemptNumber,
			DeploymentID:  deploymentID,
		},
	}, nil
}

func authorizeActorOutputReadBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "actor_output_read_authority_unavailable",
				message:   errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(auth.PermissionActorsRead, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionActorsRead) {
			return nil
		}
	}
	return forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()})
}

func (s *Server) writeActorOutputReadScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) || isScopeRequestError(err) {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	s.writeActorOutputReadAuthorityError(w)
}

func (s *Server) writeActorOutputReadAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code:      "actor_output_read_authority_unavailable",
		message:   "Actor output read authority is unavailable",
		retryable: true,
	}))
}

func writeActorOutputReadAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Actor output read authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "actor_output_read_authority_unavailable",
			message:   "Actor output read authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code: "authentication_required", message: "authentication is required",
	}))
}
