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
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	sessionOutputDefaultLimit = int32(50)
	sessionOutputMaxLimit     = int32(100)
	maxSessionOutputSequence  = int64(1<<53 - 1)
	maxSessionOutputFrontier  = int64(1 << 53)
)

type sessionOutputReadRequest struct {
	after *int64
	limit int32
}

func (s *Server) readSessionOutputHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	request, err := parseSessionOutputPageOptions(r)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_output_read", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeSessionOutputReadBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	scope, environmentID, err := s.sessionReadScope(r, principal)
	if err != nil {
		s.writeSessionOutputReadScopeError(w, err)
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsRead, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return
	}
	if s.db == nil {
		s.writeSessionOutputReadAuthorityError(w)
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
		s.writeSessionOutputReadAuthorityError(w)
		return
	}
	response, err := readSessionOutputPage(
		r.Context(), s.db, environmentID, actor.ID, request.after, request.limit,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, notFound(codedError{code: "session_not_found", message: "session not found"}))
			return
		}
		s.writeSessionOutputReadAuthorityError(w)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func parseSessionOutputPageOptions(r *http.Request) (sessionOutputReadRequest, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return sessionOutputReadRequest{}, errors.New("query string is malformed")
	}
	for name, entries := range values {
		if name != "after" && name != "limit" {
			return sessionOutputReadRequest{}, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return sessionOutputReadRequest{}, fmt.Errorf("query parameter %q must appear exactly once with a non-empty value", name)
		}
	}
	request := sessionOutputReadRequest{limit: sessionOutputDefaultLimit}
	if entries, ok := values["after"]; ok {
		value, err := parseSessionOutputDecimal(entries[0], 0, maxSessionOutputSequence, "after")
		if err != nil {
			return sessionOutputReadRequest{}, err
		}
		request.after = &value
	}
	if entries, ok := values["limit"]; ok {
		value, err := parseSessionOutputDecimal(entries[0], 1, int64(sessionOutputMaxLimit), "limit")
		if err != nil {
			return sessionOutputReadRequest{}, err
		}
		request.limit = int32(value)
	}
	return request, nil
}

func readSessionOutputPage(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	sessionID pgtype.UUID,
	after *int64,
	limit int32,
) (api.SessionOutputPage, error) {
	var afterSequence int64
	afterPresent := after != nil
	if afterPresent {
		afterSequence = *after
	}
	rows, err := store.ReadPublicActorOutputPage(ctx, db.ReadPublicActorOutputPageParams{
		LimitCount: limit + 1, AfterPresent: afterPresent,
		AfterSequence: afterSequence, EnvironmentID: environmentID,
		SessionID: sessionID,
	})
	if err != nil {
		return api.SessionOutputPage{}, err
	}
	if len(rows) == 0 {
		return api.SessionOutputPage{}, pgx.ErrNoRows
	}
	first := rows[0]
	if first.NextOutputSequence < 1 ||
		first.NextOutputSequence > maxSessionOutputFrontier ||
		first.EffectiveAfter < 0 ||
		first.EffectiveAfter > maxSessionOutputSequence {
		return api.SessionOutputPage{}, errors.New("session output projection is invalid")
	}
	response := api.SessionOutputPage{
		Records:   make([]api.SessionOutput, 0, min(len(rows), int(limit))),
		NextAfter: first.EffectiveAfter,
	}
	for _, row := range rows {
		if row.SessionID != first.SessionID ||
			row.NextOutputSequence != first.NextOutputSequence ||
			row.EffectiveAfter != first.EffectiveAfter {
			return api.SessionOutputPage{}, errors.New("session output projection is inconsistent")
		}
		if !row.RecordID.Valid {
			if len(rows) != 1 {
				return api.SessionOutputPage{}, errors.New("session output empty projection is inconsistent")
			}
			break
		}
		record, err := projectSessionOutput(row)
		if err != nil {
			return api.SessionOutputPage{}, err
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

func parseSessionOutputDecimal(raw string, minimum, maximum int64, name string) (int64, error) {
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

func projectSessionOutput(row db.ReadPublicActorOutputPageRow) (api.SessionOutput, error) {
	recordUUID := uuid.UUID(row.RecordID.Bytes)
	recordID := recordUUID.String()
	if err := ids.Validate(recordID); err != nil {
		return api.SessionOutput{}, err
	}
	if row.Sequence <= row.EffectiveAfter ||
		row.Sequence >= row.NextOutputSequence ||
		row.Sequence > maxSessionOutputSequence ||
		!json.Valid(row.Data) ||
		row.ContentType == "" ||
		!row.CreatedAt.Valid ||
		row.ProducerAttemptNumber < 1 {
		return api.SessionOutput{}, errors.New("session output record projection is invalid")
	}
	runID := pgvalue.UUIDString(row.RunID)
	if err := ids.Validate(runID); err != nil {
		return api.SessionOutput{}, errors.New("session output producer run ID is invalid")
	}
	deploymentID := pgvalue.UUIDString(row.DeploymentID)
	if err := ids.Validate(deploymentID); err != nil {
		return api.SessionOutput{}, errors.New("session output deployment ID is invalid")
	}
	return api.SessionOutput{
		ID:          recordID,
		Sequence:    row.Sequence,
		Data:        append(json.RawMessage(nil), row.Data...),
		ContentType: row.ContentType,
		CreatedAt:   row.CreatedAt.Time.UTC(),
		Provenance: api.SessionOutputProvenance{
			RunID:         runID,
			AttemptNumber: row.ProducerAttemptNumber,
			DeploymentID:  deploymentID,
		},
	}, nil
}

func authorizeSessionOutputReadBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "session_output_read_authority_unavailable",
				message:   errAPIKeyEnvironmentScopeRequired.Error(),
				retryable: true,
			})
		}
		if principal.HasPermission(auth.PermissionSessionsRead, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, auth.PermissionSessionsRead) {
			return nil
		}
	}
	return forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()})
}

func (s *Server) writeSessionOutputReadScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	s.writeSessionOutputReadAuthorityError(w)
}

func (s *Server) writeSessionOutputReadAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code:      "session_output_read_authority_unavailable",
		message:   "session output read authority is unavailable",
		retryable: true,
	}))
}

func writeSessionOutputReadAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Session output read authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "session_output_read_authority_unavailable",
			message:   "session output read authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code: "authentication_required", message: "authentication is required",
	}))
}
