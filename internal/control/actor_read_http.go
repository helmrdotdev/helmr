package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type actorReadAddress struct {
	publicID string
	key      string
}

type actorReadRecord struct {
	publicID           string
	key                pgtype.Text
	state              string
	createdAt          pgtype.Timestamptz
	updatedAt          pgtype.Timestamptz
	currentRunID       pgtype.UUID
	currentRunPublicID pgtype.Text
	failureCode        pgtype.Text
	failureRunID       pgtype.UUID
	failureRunPublicID pgtype.Text
}

func (s *Server) getActorStatusHTTP(w http.ResponseWriter, r *http.Request) {
	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	address, err := parseActorReadAddress(r)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	if err := authorizeActorReadBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	scope, environmentID, err := s.actorReadScope(r, principal)
	if err != nil {
		s.writeActorReadScopeError(w, err)
		return
	}
	if !principal.HasPermission(auth.PermissionActorsRead, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return
	}
	if s.db == nil {
		s.writeActorReadAuthorityError(w)
		return
	}
	status, err := getActorStatus(
		r.Context(), s.db, environmentID, actorDeclaredID, address,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "actor_not_found", message: "Actor not found"}))
		return
	}
	if err != nil {
		s.writeActorReadAuthorityError(w)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func getActorStatus(
	ctx context.Context,
	store db.Querier,
	environmentID pgtype.UUID,
	actorDeclaredID string,
	address actorReadAddress,
) (api.ActorStatus, error) {
	row, err := store.GetActorRead(ctx, db.GetActorReadParams{
		EnvironmentID: environmentID, ActorDeclaredID: actorDeclaredID,
		AddressPublicID: pgvalue.Text(address.publicID),
		AddressKey:      pgvalue.Text(address.key),
	})
	if err != nil {
		return api.ActorStatus{}, err
	}
	return projectActorStatus(actorReadRecordFromGet(row))
}

func parseActorReadAddress(r *http.Request) (actorReadAddress, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return actorReadAddress{}, errors.New("query string is malformed")
	}
	for name, entries := range values {
		if name != "actor_id" && name != "actor_key" {
			return actorReadAddress{}, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 {
			return actorReadAddress{}, fmt.Errorf("query parameter %q must appear exactly once", name)
		}
	}
	publicID, hasID, err := singleNonEmptyQueryValue(values, "actor_id")
	if err != nil {
		return actorReadAddress{}, err
	}
	key, hasKey, err := singleNonEmptyQueryValue(values, "actor_key")
	if err != nil {
		return actorReadAddress{}, err
	}
	if hasID == hasKey {
		return actorReadAddress{}, errors.New("exactly one of actor_id or actor_key is required")
	}
	if hasID {
		if err := api.ValidateActorPublicID(publicID); err != nil {
			return actorReadAddress{}, err
		}
		return actorReadAddress{publicID: publicID}, nil
	}
	if err := api.ValidateActorKey(key); err != nil {
		return actorReadAddress{}, err
	}
	return actorReadAddress{key: key}, nil
}

func singleNonEmptyQueryValue(values map[string][]string, name string) (string, bool, error) {
	entries, ok := values[name]
	if !ok {
		return "", false, nil
	}
	if len(entries) != 1 || entries[0] == "" {
		return "", false, fmt.Errorf("%s must be a non-empty string", name)
	}
	return entries[0], true, nil
}

func authorizeActorReadBeforeLookup(principal auth.Actor) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		scope, ok := principal.EnvironmentScope()
		if !ok {
			return unavailable(codedError{
				code:      "actor_read_authority_unavailable",
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

func (s *Server) actorReadScope(
	r *http.Request,
	principal auth.Actor,
) (auth.Scope, pgtype.UUID, error) {
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, err
	}
	scope, _, environmentID, err := s.requestEnvironmentScope(r.Context(), principal, projectRef, environmentRef)
	return scope, environmentID, err
}

func (s *Server) writeActorReadScopeError(w http.ResponseWriter, err error) {
	if isInvalidEnvironmentScopeReference(err) || isScopeRequestError(err) {
		writeError(w, badRequest(codedError{code: "invalid_actor_reference", message: err.Error()}))
		return
	}
	s.writeActorReadAuthorityError(w)
}

func (s *Server) writeActorReadAuthorityError(w http.ResponseWriter) {
	writeError(w, unavailable(codedError{
		code:      "actor_read_authority_unavailable",
		message:   "Actor read authority is unavailable",
		retryable: true,
	}))
}

func writeActorReadAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Actor read authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "actor_read_authority_unavailable",
			message:   "Actor read authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code: "authentication_required", message: "authentication is required",
	}))
}

func projectActorStatus(record actorReadRecord) (api.ActorStatus, error) {
	if err := api.ValidateActorPublicID(record.publicID); err != nil {
		return api.ActorStatus{}, err
	}
	status, err := actorPublicStatus(record.state)
	if err != nil {
		return api.ActorStatus{}, err
	}
	if !record.createdAt.Valid || !record.updatedAt.Valid {
		return api.ActorStatus{}, errors.New("Actor timestamps are unavailable")
	}
	if record.currentRunID.Valid != record.currentRunPublicID.Valid {
		return api.ActorStatus{}, errors.New("Actor current Run projection is inconsistent")
	}
	failed := status == api.ActorPublicStatusFailed
	if failed != (record.failureCode.Valid && record.failureRunID.Valid && record.failureRunPublicID.Valid) {
		return api.ActorStatus{}, errors.New("Actor failure projection is inconsistent")
	}
	if !failed && (record.failureCode.Valid || record.failureRunID.Valid || record.failureRunPublicID.Valid) {
		return api.ActorStatus{}, errors.New("Actor failure projection is inconsistent")
	}
	if record.currentRunPublicID.Valid {
		if err := publicid.ValidateFor(publicid.Run, record.currentRunPublicID.String); err != nil {
			return api.ActorStatus{}, errors.New("Actor current Run public ID is invalid")
		}
	}
	if record.failureRunPublicID.Valid {
		if err := publicid.ValidateFor(publicid.Run, record.failureRunPublicID.String); err != nil {
			return api.ActorStatus{}, errors.New("Actor failure Run public ID is invalid")
		}
	}
	if record.failureCode.Valid {
		switch record.failureCode.String {
		case "no-progress", "run-failed", "run-expired", "platform-failure":
		default:
			return api.ActorStatus{}, errors.New("Actor failure code is invalid")
		}
	}
	result := api.ActorStatus{
		ID:        record.publicID,
		Status:    status,
		CreatedAt: record.createdAt.Time.UTC(),
		UpdatedAt: record.updatedAt.Time.UTC(),
	}
	if record.key.Valid {
		result.Key = &record.key.String
	}
	if record.currentRunPublicID.Valid {
		result.CurrentRunID = &record.currentRunPublicID.String
	}
	if failed {
		result.Failure = &api.ActorFailure{
			Code: record.failureCode.String, RunID: record.failureRunPublicID.String,
		}
	}
	return result, nil
}

func actorPublicStatus(state string) (api.ActorPublicStatus, error) {
	switch state {
	case "open", "closing":
		return api.ActorPublicStatusOpen, nil
	case "closed":
		return api.ActorPublicStatusClosed, nil
	case "cancelling", "cancelled":
		return api.ActorPublicStatusCancelled, nil
	case "failed":
		return api.ActorPublicStatusFailed, nil
	default:
		return "", fmt.Errorf("Actor state %q has no public status", state)
	}
}

func actorReadRecordFromGet(row db.GetActorReadRow) actorReadRecord {
	return actorReadRecord{
		publicID: row.PublicID, key: row.Key, state: row.State,
		createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
		currentRunID: row.CurrentRunID, currentRunPublicID: row.CurrentRunPublicID,
		failureCode: row.FailureCode, failureRunID: row.FailureRunID,
		failureRunPublicID: row.FailureRunPublicID,
	}
}
