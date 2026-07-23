package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	actorListDefaultLimit = int32(50)
	actorListMaxLimit     = int32(100)
	actorListCursorPrefix = "ac1."
)

type actorReadAddress struct {
	publicID string
	key      string
}

type actorListRequest struct {
	lifecycle string
	cursor    string
	limit     int32
}

type actorListCursor struct {
	ProjectID       string `json:"project_id"`
	EnvironmentID   string `json:"environment_id"`
	ActorDeclaredID string `json:"actor_declared_id"`
	CreatedAt       string `json:"created_at"`
	ActorID         string `json:"actor_id"`
}

type actorReadRecord struct {
	publicID                  string
	key                       pgtype.Text
	state                     string
	expiresAt                 pgtype.Timestamptz
	metadata                  []byte
	tags                      []string
	managedQueueName          string
	managedConcurrencyKey     pgtype.Text
	managedPriority           int32
	managedQueuedTTLMS        pgtype.Int8
	managedMaxDurationMS      int64
	managedRetryPolicyVersion int32
	managedRetryPolicy        []byte
	managedRunMetadata        []byte
	managedRunTags            []string
	createdAt                 pgtype.Timestamptz
	updatedAt                 pgtype.Timestamptz
	currentRunID              pgtype.UUID
	currentRunPublicID        pgtype.Text
	failureCode               pgtype.Text
	failureRunID              pgtype.UUID
	failureRunPublicID        pgtype.Text
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
	scope, _, environmentID, err := s.actorReadScope(r, principal)
	if err != nil {
		s.writeActorReadScopeError(w, err, "invalid_actor_reference")
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
	row, err := s.db.GetActorRead(r.Context(), db.GetActorReadParams{
		EnvironmentID:   environmentID,
		ActorDeclaredID: actorDeclaredID,
		AddressPublicID: pgvalue.Text(address.publicID),
		AddressKey:      pgvalue.Text(address.key),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "actor_not_found", message: "Actor not found"}))
		return
	}
	if err != nil {
		s.writeActorReadAuthorityError(w)
		return
	}
	status, err := projectActorStatus(actorReadRecordFromGet(row))
	if err != nil {
		s.writeActorReadAuthorityError(w)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) listActorsHTTP(w http.ResponseWriter, r *http.Request) {
	actorDeclaredID := chi.URLParam(r, "actorDeclaredID")
	if err := api.ValidateActorDeclaredID(actorDeclaredID); err != nil {
		writeError(w, badRequest(codedError{code: "invalid_actor_list", message: err.Error()}))
		return
	}
	request, err := parseActorListRequest(r)
	if err != nil {
		var cursorError actorCursorError
		if errors.As(err, &cursorError) {
			writeError(w, badRequest(codedError{code: "invalid_actor_cursor", message: err.Error()}))
			return
		}
		writeError(w, badRequest(codedError{code: "invalid_actor_list", message: err.Error()}))
		return
	}
	principal := actorFromContext(r.Context())
	if err := authorizeActorReadBeforeLookup(principal); err != nil {
		writeError(w, err)
		return
	}
	scope, _, environmentID, err := s.actorReadScope(r, principal)
	if err != nil {
		s.writeActorReadScopeError(w, err, "invalid_actor_list")
		return
	}
	if !principal.HasPermission(auth.PermissionActorsRead, scope) {
		writeError(w, forbidden(codedError{
			code: "permission_required", message: errPermissionRequired.Error(),
		}))
		return
	}

	var cursorCreatedAt pgtype.Timestamptz
	var cursorPublicID pgtype.Text
	if request.cursor != "" {
		cursor, err := parseActorListCursor(
			request.cursor,
			scope.ProjectID,
			scope.EnvironmentID,
			actorDeclaredID,
		)
		if err != nil {
			writeError(w, badRequest(codedError{code: "invalid_actor_cursor", message: err.Error()}))
			return
		}
		cursorCreatedAt = pgtype.Timestamptz{Time: cursor.createdAt, Valid: true}
		cursorPublicID = pgvalue.Text(cursor.actorID)
	}
	if s.db == nil {
		s.writeActorReadAuthorityError(w)
		return
	}
	rows, err := s.db.ListActorReads(r.Context(), db.ListActorReadsParams{
		EnvironmentID:   environmentID,
		ActorDeclaredID: actorDeclaredID,
		Lifecycle:       pgvalue.Text(request.lifecycle),
		CursorCreatedAt: cursorCreatedAt,
		CursorPublicID:  cursorPublicID,
		LimitCount:      request.limit + 1,
	})
	if err != nil {
		s.writeActorReadAuthorityError(w)
		return
	}
	hasMore := len(rows) > int(request.limit)
	if hasMore {
		rows = rows[:request.limit]
	}
	response := api.ListActorsResponse{Actors: make([]api.ActorStatus, 0, len(rows))}
	for _, row := range rows {
		status, err := projectActorStatus(actorReadRecordFromList(row))
		if err != nil {
			s.writeActorReadAuthorityError(w)
			return
		}
		response.Actors = append(response.Actors, status)
	}
	if hasMore {
		last := rows[len(rows)-1]
		nextCursor, err := encodeActorListCursor(actorListCursor{
			ProjectID:       scope.ProjectID,
			EnvironmentID:   scope.EnvironmentID,
			ActorDeclaredID: actorDeclaredID,
			CreatedAt:       last.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			ActorID:         last.PublicID,
		})
		if err != nil {
			s.writeActorReadAuthorityError(w)
			return
		}
		response.NextCursor = nextCursor
	}
	writeJSON(w, http.StatusOK, response)
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

type actorCursorError struct {
	message string
}

func (e actorCursorError) Error() string {
	return e.message
}

func parseActorListRequest(r *http.Request) (actorListRequest, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		if rawQueryHasParameter(r.URL.RawQuery, "cursor") {
			return actorListRequest{}, actorCursorError{message: "cursor is malformed"}
		}
		return actorListRequest{}, fmt.Errorf("query string is malformed: %w", err)
	}
	for name, entries := range values {
		switch name {
		case "lifecycle", "cursor", "limit":
		default:
			return actorListRequest{}, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 {
			return actorListRequest{}, fmt.Errorf("query parameter %q must appear exactly once", name)
		}
		if entries[0] == "" {
			if name == "cursor" {
				return actorListRequest{}, actorCursorError{message: "cursor must be non-empty when present"}
			}
			return actorListRequest{}, fmt.Errorf("%s must be non-empty when present", name)
		}
	}
	request := actorListRequest{limit: actorListDefaultLimit}
	if entries, ok := values["lifecycle"]; ok {
		request.lifecycle = entries[0]
		if err := api.ValidateActorLifecycle(request.lifecycle); err != nil {
			return actorListRequest{}, err
		}
	}
	if entries, ok := values["cursor"]; ok {
		request.cursor = entries[0]
	}
	if entries, ok := values["limit"]; ok {
		value, err := strconv.ParseInt(entries[0], 10, 32)
		if err != nil || value < 1 || value > int64(actorListMaxLimit) {
			return actorListRequest{}, fmt.Errorf("limit must be an integer in [1,%d]", actorListMaxLimit)
		}
		request.limit = int32(value)
	}
	return request, nil
}

func rawQueryHasParameter(rawQuery string, name string) bool {
	for _, pair := range strings.Split(rawQuery, "&") {
		rawName, _, _ := strings.Cut(pair, "=")
		decodedName, err := url.QueryUnescape(rawName)
		if err == nil && decodedName == name {
			return true
		}
	}
	return false
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
) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectRef, environmentRef, err := environmentScopeRefsFromRequest(r, principal, "", "")
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return s.requestEnvironmentScope(r.Context(), principal, projectRef, environmentRef)
}

func (s *Server) writeActorReadScopeError(w http.ResponseWriter, err error, invalidCode string) {
	if isInvalidEnvironmentScopeReference(err) || isScopeRequestError(err) {
		writeError(w, badRequest(codedError{code: invalidCode, message: err.Error()}))
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

type parsedActorListCursor struct {
	createdAt time.Time
	actorID   string
}

func encodeActorListCursor(cursor actorListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return actorListCursorPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseActorListCursor(
	raw string,
	projectID string,
	environmentID string,
	actorDeclaredID string,
) (parsedActorListCursor, error) {
	if !strings.HasPrefix(raw, actorListCursorPrefix) {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor has an unsupported version"}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, actorListCursorPrefix))
	if err != nil {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor is malformed"}
	}
	var cursor actorListCursor
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor is malformed"}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor is malformed"}
	}
	if cursor.ProjectID != projectID ||
		cursor.EnvironmentID != environmentID ||
		cursor.ActorDeclaredID != actorDeclaredID {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor does not belong to this list scope"}
	}
	if err := api.ValidateActorPublicID(cursor.ActorID); err != nil {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor is malformed"}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
	if err != nil || createdAt.Format(time.RFC3339Nano) != cursor.CreatedAt {
		return parsedActorListCursor{}, actorCursorError{message: "Actor cursor is malformed"}
	}
	return parsedActorListCursor{createdAt: createdAt, actorID: cursor.ActorID}, nil
}

func projectActorStatus(record actorReadRecord) (api.ActorStatus, error) {
	if err := api.ValidateActorPublicID(record.publicID); err != nil {
		return api.ActorStatus{}, err
	}
	if err := api.ValidateActorLifecycle(record.state); err != nil {
		return api.ActorStatus{}, err
	}
	if !record.createdAt.Valid || !record.updatedAt.Valid {
		return api.ActorStatus{}, errors.New("Actor timestamps are unavailable")
	}
	if record.currentRunID.Valid != record.currentRunPublicID.Valid {
		return api.ActorStatus{}, errors.New("Actor current Run projection is inconsistent")
	}
	failed := record.state == string(api.ActorLifecycleFailed)
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
	if record.managedRetryPolicyVersion != 0 {
		return api.ActorStatus{}, errors.New("Actor retry policy version is unsupported")
	}
	retryManifest, err := deployment.ParseRetryManifest(record.managedRetryPolicy)
	if err != nil {
		return api.ActorStatus{}, fmt.Errorf("decode Actor retry policy: %w", err)
	}
	retry := api.ActorManagedRetryPolicy{Enabled: retryManifest.Enabled}
	if retryManifest.Enabled {
		minDelay, err := formatDurationMilliseconds(retryManifest.Backoff.MinMs)
		if err != nil {
			return api.ActorStatus{}, err
		}
		maxDelay, err := formatDurationMilliseconds(retryManifest.Backoff.MaxMs)
		if err != nil {
			return api.ActorStatus{}, err
		}
		retry.MaxAttempts = retryManifest.MaxAttempts
		retry.Backoff = &api.ActorManagedRetryBackoff{
			MinDelay: minDelay,
			MaxDelay: maxDelay,
			Factor:   retryManifest.Backoff.Factor,
			Jitter:   string(retryManifest.Backoff.Jitter),
		}
	}
	maxDuration, err := formatDurationMilliseconds(record.managedMaxDurationMS)
	if err != nil {
		return api.ActorStatus{}, err
	}
	var ttl *string
	if record.managedQueuedTTLMS.Valid {
		value, err := formatDurationMilliseconds(record.managedQueuedTTLMS.Int64)
		if err != nil {
			return api.ActorStatus{}, err
		}
		ttl = &value
	}
	metadata, err := validatedJSONObjectCopy(record.metadata, "Actor metadata")
	if err != nil {
		return api.ActorStatus{}, err
	}
	runMetadata, err := validatedJSONObjectCopy(record.managedRunMetadata, "Actor managed Run metadata")
	if err != nil {
		return api.ActorStatus{}, err
	}
	status := api.ActorStatus{
		ID:        record.publicID,
		Lifecycle: api.ActorLifecycle(record.state),
		Metadata:  metadata,
		Tags:      nonNilStrings(record.tags),
		Run: api.ActorManagedRunOptions{
			Queue:       record.managedQueueName,
			Priority:    record.managedPriority,
			TTL:         ttl,
			MaxDuration: maxDuration,
			Retry:       retry,
			Metadata:    runMetadata,
			Tags:        nonNilStrings(record.managedRunTags),
		},
		CreatedAt: record.createdAt.Time.UTC(),
		UpdatedAt: record.updatedAt.Time.UTC(),
	}
	if record.key.Valid {
		status.Key = &record.key.String
	}
	if record.expiresAt.Valid {
		expiresAt := record.expiresAt.Time.UTC()
		status.ExpiresAt = &expiresAt
	}
	if record.managedConcurrencyKey.Valid {
		status.Run.ConcurrencyKey = &record.managedConcurrencyKey.String
	}
	if record.currentRunPublicID.Valid {
		status.CurrentRunID = &record.currentRunPublicID.String
	}
	if failed {
		status.Failure = &api.ActorFailure{
			Code: record.failureCode.String, RunID: record.failureRunPublicID.String,
		}
	}
	return status, nil
}

func validatedJSONObjectCopy(raw []byte, label string) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("%s is invalid", label)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s is not an object", label)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func nonNilStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func formatDurationMilliseconds(value int64) (string, error) {
	if value <= 0 {
		return "", errors.New("duration milliseconds must be positive")
	}
	for _, unit := range []struct {
		milliseconds int64
		suffix       string
	}{
		{24 * 60 * 60 * 1000, "d"},
		{60 * 60 * 1000, "h"},
		{60 * 1000, "m"},
		{1000, "s"},
		{1, "ms"},
	} {
		if value%unit.milliseconds == 0 {
			return strconv.FormatInt(value/unit.milliseconds, 10) + unit.suffix, nil
		}
	}
	panic("millisecond unit must divide every integer millisecond duration")
}

func actorReadRecordFromGet(row db.GetActorReadRow) actorReadRecord {
	return actorReadRecord{
		publicID: row.PublicID, key: row.Key, state: row.State,
		expiresAt: row.ExpiresAt, metadata: row.Metadata, tags: row.Tags,
		managedQueueName:          row.ManagedQueueName,
		managedConcurrencyKey:     row.ManagedConcurrencyKey,
		managedPriority:           row.ManagedPriority,
		managedQueuedTTLMS:        row.ManagedQueuedTtlMs,
		managedMaxDurationMS:      row.ManagedMaxActiveDurationMs,
		managedRetryPolicyVersion: row.ManagedRetryPolicyVersion,
		managedRetryPolicy:        row.ManagedRetryPolicy,
		managedRunMetadata:        row.ManagedRunMetadata,
		managedRunTags:            row.ManagedRunTags,
		createdAt:                 row.CreatedAt, updatedAt: row.UpdatedAt,
		currentRunID: row.CurrentRunID, currentRunPublicID: row.CurrentRunPublicID,
		failureCode: row.FailureCode, failureRunID: row.FailureRunID,
		failureRunPublicID: row.FailureRunPublicID,
	}
}

func actorReadRecordFromList(row db.ListActorReadsRow) actorReadRecord {
	return actorReadRecord{
		publicID: row.PublicID, key: row.Key, state: row.State,
		expiresAt: row.ExpiresAt, metadata: row.Metadata, tags: row.Tags,
		managedQueueName:          row.ManagedQueueName,
		managedConcurrencyKey:     row.ManagedConcurrencyKey,
		managedPriority:           row.ManagedPriority,
		managedQueuedTTLMS:        row.ManagedQueuedTtlMs,
		managedMaxDurationMS:      row.ManagedMaxActiveDurationMs,
		managedRetryPolicyVersion: row.ManagedRetryPolicyVersion,
		managedRetryPolicy:        row.ManagedRetryPolicy,
		managedRunMetadata:        row.ManagedRunMetadata,
		managedRunTags:            row.ManagedRunTags,
		createdAt:                 row.CreatedAt, updatedAt: row.UpdatedAt,
		currentRunID: row.CurrentRunID, currentRunPublicID: row.CurrentRunPublicID,
		failureCode: row.FailureCode, failureRunID: row.FailureRunID,
		failureRunPublicID: row.FailureRunPublicID,
	}
}
