package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) readSessionInputHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
		return
	}
	request, err := parseSessionRecordPageOptions(r)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_session_input_read", message: err.Error()}))
		return
	}

	principal := actorFromContext(r.Context())
	if err := authorizeSessionRecordReadBeforeLookup(principal, "session_authority_unavailable"); err != nil {
		writeError(w, err)
		return
	}
	scope, environmentID, err := s.sessionReadScope(r, principal)
	if err != nil {
		if isInvalidEnvironmentScopeReference(err) {
			writeError(w, badRequest(codedError{code: "invalid_session_id", message: err.Error()}))
			return
		}
		s.writeSessionReadAuthorityError(w, err)
		return
	}
	if !principal.HasPermission(auth.PermissionSessionsRead, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return
	}
	if s.db == nil {
		s.writeSessionReadAuthorityError(w, errors.New("database is unavailable"))
		return
	}

	response, err := readSessionInputPage(
		r.Context(), s.db, environmentID, pgvalue.UUID(sessionID), request.after, request.limit,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "session_not_found", message: "session not found"}))
		return
	}
	if err != nil {
		s.writeSessionReadAuthorityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func readSessionInputPage(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	sessionID pgtype.UUID,
	after *int64,
	limit int32,
) (api.SessionInputPage, error) {
	var afterSequence int64
	afterPresent := after != nil
	if afterPresent {
		afterSequence = *after
	}
	rows, err := store.ReadPublicActorInputPage(ctx, db.ReadPublicActorInputPageParams{
		LimitCount: limit + 1, AfterPresent: afterPresent,
		AfterSequence: afterSequence, EnvironmentID: environmentID,
		SessionID: sessionID,
	})
	if err != nil {
		return api.SessionInputPage{}, err
	}
	if len(rows) == 0 {
		return api.SessionInputPage{}, pgx.ErrNoRows
	}
	first := rows[0]
	if first.NextInputSequence < 1 ||
		first.NextInputSequence > maxSessionRecordFrontier ||
		first.EffectiveAfter < 0 ||
		first.EffectiveAfter > maxSessionRecordSequence {
		return api.SessionInputPage{}, errors.New("session input projection is invalid")
	}
	response := api.SessionInputPage{
		Records:   make([]api.SessionInput, 0, min(len(rows), int(limit))),
		NextAfter: first.EffectiveAfter,
	}
	for _, row := range rows {
		if row.SessionID != first.SessionID ||
			row.NextInputSequence != first.NextInputSequence ||
			row.EffectiveAfter != first.EffectiveAfter {
			return api.SessionInputPage{}, errors.New("session input projection is inconsistent")
		}
		if !row.RecordID.Valid {
			if len(rows) != 1 {
				return api.SessionInputPage{}, errors.New("session input empty projection is inconsistent")
			}
			break
		}
		if row.Sequence <= row.EffectiveAfter || row.Sequence >= row.NextInputSequence {
			return api.SessionInputPage{}, errors.New("session input record projection is invalid")
		}
		record, err := projectSessionInput(db.SessionRecord{
			ID: row.RecordID, SessionID: row.SessionID, Direction: "input",
			Sequence: row.Sequence, Data: append(json.RawMessage(nil), row.Data...),
			SourceKind:  pgtype.Text{String: row.SourceKind, Valid: row.SourceKind != ""},
			SourceRunID: row.SourceRunID, CreatedAt: row.CreatedAt,
		})
		if err != nil {
			return api.SessionInputPage{}, err
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
