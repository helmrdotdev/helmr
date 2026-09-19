package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

const sessionDataBodyLimit = int64((6 << 20) + 8192)
const sessionControlBodyLimit = int64(8 << 10)
const maxSessionEventSequence = int64(1<<53 - 1)

func (s *Server) sendSessionHTTP(w http.ResponseWriter, r *http.Request) {
	s.admitSessionHTTP(w, r, session.SendMessageOrEnqueue)
}
func (s *Server) enqueueSessionHTTP(w http.ResponseWriter, r *http.Request) {
	s.admitSessionHTTP(w, r, session.EnqueueOnly)
}
func (s *Server) sendSessionMessageHTTP(w http.ResponseWriter, r *http.Request) {
	s.admitSessionHTTP(w, r, session.ExactMessage)
}
func (s *Server) admitSessionHTTP(w http.ResponseWriter, r *http.Request, mode session.AdmissionMode) {
	var body api.SessionDataRequest
	if err := decodeSessionCommand(r, &body); err != nil {
		writeSessionRequestError(w, err)
		return
	}
	if len(body.Data) == 0 {
		writeSessionRequestError(w, errors.New("data is required"))
		return
	}
	data := body.Data // The envelope decoder already canonicalized the application JSON.
	if len(data) > 1<<20 {
		writeError(w, tooLarge(codedError{code: "invalid_request", message: "data exceeds the 1 MiB limit"}))
		return
	}
	command, err := s.sessionCommand(r, auth.PermissionSessionsSend, body.IdempotencyKey)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	request := session.AdmissionRequest{Target: command.Target, Mode: mode, Data: data, IdempotencyKey: command.IdempotencyKey}
	if mode == session.ExactMessage {
		request.TurnID, err = ids.Parse(chi.URLParam(r, "turnID"))
		if err != nil {
			writeSessionRequestError(w, err)
			return
		}
	}
	receipt, err := s.applySessionAdmission(r.Context(), request)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	if mode == session.ExactMessage {
		if receipt.MessageID == nil {
			s.writeSessionOperationError(w, errors.New("message admission lacks identity"))
			return
		}
		writeJSON(w, http.StatusAccepted, api.SessionMessageReceipt{ID: receipt.ID.String(), TurnID: receipt.TurnID.String(), MessageID: receipt.MessageID.String(), Status: "accepted"})
		return
	}
	response := api.SessionAdmissionReceipt{ID: receipt.ID.String(), Kind: receipt.Kind, TurnID: receipt.TurnID.String()}
	if receipt.MessageID != nil {
		id := receipt.MessageID.String()
		response.MessageID = &id
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (s *Server) closeSessionHTTP(w http.ResponseWriter, r *http.Request) {
	var body api.CloseSessionRequest
	if err := decodeSessionCommand(r, &body); err != nil {
		writeSessionRequestError(w, err)
		return
	}
	request, err := s.sessionCommand(r, auth.PermissionSessionsClose, body.IdempotencyKey)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	receipt, err := s.applySessionClose(r.Context(), request)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, api.SessionCloseReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), Status: receipt.Status})
}

func (s *Server) interruptSessionTurnHTTP(w http.ResponseWriter, r *http.Request) {
	var body api.InterruptTurnRequest
	if err := decodeSessionCommand(r, &body); err != nil {
		writeSessionRequestError(w, err)
		return
	}
	command, err := s.sessionCommand(r, auth.PermissionSessionsInterrupt, body.IdempotencyKey)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	turnID, err := ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		writeSessionRequestError(w, err)
		return
	}
	receipt, err := s.applySessionInterrupt(r.Context(), session.InterruptRequest{ControlRequest: command, TurnID: turnID})
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	if receipt.TurnID == nil || receipt.HoldID == nil {
		s.writeSessionOperationError(w, errors.New("interruption receipt lacks target"))
		return
	}
	writeJSON(w, http.StatusAccepted, api.TurnInterruptReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), TurnID: receipt.TurnID.String(), HoldID: receipt.HoldID.String(), Status: receipt.Status})
}

func (s *Server) resumeSessionHTTP(w http.ResponseWriter, r *http.Request) {
	var body api.ResumeSessionRequest
	if err := decodeSessionCommand(r, &body); err != nil {
		writeSessionRequestError(w, err)
		return
	}
	holdID, err := ids.Parse(body.HoldID)
	if err != nil {
		writeSessionRequestError(w, fmt.Errorf("invalid hold_id: %w", err))
		return
	}
	command, err := s.sessionCommand(r, auth.PermissionSessionsResume, body.IdempotencyKey)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	receipt, err := s.applySessionResume(r.Context(), session.ResumeRequest{ControlRequest: command, HoldID: holdID})
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	if receipt.HoldID == nil {
		s.writeSessionOperationError(w, errors.New("resume receipt lacks hold identity"))
		return
	}
	writeJSON(w, http.StatusAccepted, api.SessionResumeReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), HoldID: receipt.HoldID.String(), Status: receipt.Status})
}

func (s *Server) recoverSessionHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := decodeRecoverSessionCommand(r)
	if err != nil {
		writeSessionRequestError(w, err)
		return
	}
	command, err := s.sessionCommand(r, auth.PermissionSessionsRecover, body.IdempotencyKey)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	request := session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: command, HoldID: uuid.MustParse(body.HoldID)}, WorkspaceVersionID: uuid.MustParse(body.WorkspaceVersionID), ReconciliationRef: body.ReconciliationRef, Disposition: body.Disposition}
	if body.TurnID != nil {
		id := uuid.MustParse(*body.TurnID)
		request.TurnID = &id
	}
	receipt, err := s.applySessionRecovery(r.Context(), request)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	if receipt.HoldID == nil {
		s.writeSessionOperationError(w, errors.New("recovery receipt lacks hold identity"))
		return
	}
	response := api.SessionRecoveryReceipt{ID: receipt.ID.String(), SessionID: receipt.SessionID.String(), HoldID: receipt.HoldID.String(), Status: receipt.Status}
	if receipt.TurnID != nil {
		id := receipt.TurnID.String()
		response.TurnID = &id
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (s *Server) sessionCommand(r *http.Request, permission auth.Permission, key string) (session.ControlRequest, error) {
	target, err := s.sessionOperationTarget(r, permission)
	if err != nil {
		return session.ControlRequest{}, err
	}
	return session.ControlRequest{Target: target, IdempotencyKey: key}, nil
}

func authorizeSessionOperation(principal auth.Actor, permission auth.Permission) error {
	switch principal.Kind {
	case auth.ActorKindAPIKey:
		if scope, ok := principal.EnvironmentScope(); ok && principal.HasPermission(permission, scope) {
			return nil
		}
	case auth.ActorKindSession:
		if auth.RoleAllows(principal.Role, permission) {
			return nil
		}
	}
	return forbidden(codedError{code: "forbidden", message: "permission is required"})
}

func (s *Server) sessionOperationTarget(r *http.Request, permission auth.Permission) (session.Target, error) {
	principal := actorFromContext(r.Context())
	if err := authorizeSessionOperation(principal, permission); err != nil {
		return session.Target{}, err
	}
	id, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		return session.Target{}, badRequest(codedError{code: "invalid_request", message: err.Error()})
	}
	if s.db == nil {
		return session.Target{}, unavailable(codedError{code: "unavailable", message: "Session storage is unavailable"})
	}
	scope, environmentID, err := s.sessionReadScope(r, principal)
	if err != nil {
		if isInvalidEnvironmentScopeReference(err) {
			return session.Target{}, badRequest(codedError{code: "invalid_request", message: err.Error()})
		}
		return session.Target{}, err
	}
	if !principal.HasPermission(permission, scope) {
		return session.Target{}, forbidden(codedError{code: "forbidden", message: "permission is required"})
	}
	return session.Target{EnvironmentID: pgvalue.MustUUIDValue(environmentID), SessionID: id}, nil
}

// Fixed-envelope decoding rejects ambiguous JSON before an operation can claim a key.
func decodeSessionCommand(r *http.Request, destination any, required ...string) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	raw, err = jsoncanon.Transform(raw)
	if err != nil {
		return err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return errors.New("request must be a JSON object")
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("%s is required", name)
		}
	}
	if value, ok := members["idempotency_key"]; ok {
		var key string
		if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &key) != nil || strings.TrimSpace(key) == "" {
			return errors.New("idempotency_key must be a non-empty string")
		}
		if _, err := normalizeIdempotencyKey(key); err != nil {
			return err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func decodeRecoverSessionCommand(r *http.Request) (api.RecoverSessionRequest, error) {
	var envelope struct {
		api.RecoverSessionRequest
		Disposition json.RawMessage `json:"disposition"`
	}
	if err := decodeSessionCommand(r, &envelope, "turn_id"); err != nil {
		return api.RecoverSessionRequest{}, err
	}
	body := envelope.RecoverSessionRequest
	if len(envelope.Disposition) > 0 {
		if body.TurnID == nil {
			return body, errors.New("null turn_id forbids disposition")
		}
		if err := json.Unmarshal(envelope.Disposition, &body.Disposition); err != nil {
			return body, errors.New("disposition must be a string")
		}
	}
	return body, api.ValidateRecoverSessionRequest(body)
}

func writeSessionRequestError(w http.ResponseWriter, err error) {
	var size *http.MaxBytesError
	if errors.As(err, &size) {
		writeError(w, tooLarge(codedError{code: "invalid_request", message: "request exceeds the size limit"}))
		return
	}
	writeError(w, badRequest(codedError{code: "invalid_request", message: err.Error()}))
}

func (s *Server) writeSessionOperationError(w http.ResponseWriter, err error) {
	var operation *session.OperationError
	var collision idempotency.ConflictError
	var transport apiError
	switch {
	case errors.As(err, &transport):
		writeError(w, err)
	case errors.As(err, &collision):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier operation"}))
	case errors.As(err, &operation):
		coded := codedError{code: operation.Code, message: operation.Code}
		switch operation.Code {
		case "session_not_found", "turn_not_found":
			writeError(w, notFound(coded))
		case "invalid_request", "invalid_cursor":
			writeError(w, badRequest(coded))
		case "forbidden":
			writeError(w, forbidden(coded))
		case "cursor_expired":
			writeError(w, gone(sessionCursorExpiredError{codedError: coded, retainedAfter: operation.RetainedAfter}))
		default:
			writeError(w, conflict(coded))
		}
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, notFound(codedError{code: "session_not_found", message: "Session not found"}))
	default:
		if s.log != nil {
			s.log.Error("Session operation failed", "error", err)
		}
		writeError(w, errors.New("Session operation failed"))
	}
}

type sessionCursorExpiredError struct {
	codedError
	retainedAfter int64
}

func (e sessionCursorExpiredError) ErrorDetails() map[string]json.RawMessage {
	return map[string]json.RawMessage{"retained_after": json.RawMessage(strconv.FormatInt(e.retainedAfter, 10))}
}

func writeSessionLifecycleAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) {
		writeError(w, unauthorized(codedError{code: "authentication_required", message: "authentication is required"}))
		return
	}
	if log != nil {
		log.Error("Session authentication failed", "error", err)
	}
	writeError(w, unavailable(codedError{code: "unavailable", message: "authentication is unavailable", retryable: true}))
}

func (s *Server) readSessionEventsHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := s.sessionOperationTarget(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	after, limit, err := parseSessionEventPageOptions(r.URL.RawQuery)
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_cursor", message: err.Error()}))
		return
	}
	page, err := session.ReadEvents(r.Context(), s.db, target, after, limit)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	response := api.SessionEventPage{Records: make([]api.SessionEvent, 0, len(page.Records)), NextAfter: page.NextAfter, HasMore: page.HasMore, RetainedAfter: page.RetainedAfter}
	for _, row := range page.Records {
		event := api.SessionEvent{ID: pgvalue.UUIDString(row.ID), SessionID: pgvalue.UUIDString(row.SessionID), Sequence: row.Sequence, CreatedAt: row.CreatedAt.Time.UTC(), Kind: row.Kind, Data: row.Data}
		if row.TurnID.Valid {
			id := pgvalue.UUIDString(row.TurnID)
			event.TurnID = &id
		}
		if row.ProducerRunID.Valid {
			event.Provenance = &api.SessionEventProvenance{RunID: pgvalue.UUIDString(row.ProducerRunID), AttemptNumber: row.ProducerAttemptNumber.Int32, RunGeneration: row.RunGeneration.Int64, DeploymentID: pgvalue.UUIDString(row.DeploymentID)}
		}
		response.Records = append(response.Records, event)
	}
	writeJSON(w, http.StatusOK, response)
}

func parseSessionEventPageOptions(raw string) (int64, int32, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return 0, 0, err
	}
	after, limit := int64(0), int64(100)
	for name, entries := range values {
		if name != "after" && name != "limit" {
			return 0, 0, fmt.Errorf("unsupported query parameter %q", name)
		}
		if len(entries) != 1 || entries[0] == "" {
			return 0, 0, fmt.Errorf("%s must appear once with a value", name)
		}
		for _, c := range entries[0] {
			if c < '0' || c > '9' {
				return 0, 0, fmt.Errorf("%s must be a decimal integer", name)
			}
		}
		n, err := strconv.ParseInt(entries[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid %s", name)
		}
		if name == "after" {
			after = n
		} else {
			limit = n
		}
	}
	if after < 0 || after > maxSessionEventSequence || limit < 1 || limit > 1000 {
		return 0, 0, errors.New("after must be a safe non-negative integer and limit must be in [1,1000]")
	}
	return after, int32(limit), nil
}

func (s *Server) getSessionTurnHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := s.sessionOperationTarget(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	turnID, err := ids.Parse(chi.URLParam(r, "turnID"))
	if err != nil {
		writeSessionRequestError(w, err)
		return
	}
	view, err := session.GetTurn(r.Context(), s.db, target, turnID)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	response, err := projectSessionTurn(view)
	if err != nil {
		s.writeSessionOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func projectSessionTurn(view session.TurnView) (api.SessionTurn, error) {
	row := view.Turn
	response := api.SessionTurn{ID: pgvalue.UUIDString(row.ID), SessionID: pgvalue.UUIDString(row.SessionID), Sequence: row.Sequence, Input: row.Data, Source: api.SessionTurnSource{Type: "external"}, Status: row.Status, CreatedAt: row.CreatedAt.Time.UTC(), InterruptRequested: row.InterruptRequestedAt.Valid, AcceptsMessages: view.AcceptsMessages}
	if row.SourceRunID.Valid {
		response.Source = api.SessionTurnSource{Type: "run", RunID: pgvalue.UUIDString(row.SourceRunID)}
	}
	if event := view.TerminalEvent; event != nil {
		id, version := pgvalue.UUIDString(event.ID), pgvalue.UUIDString(event.WorkspaceVersionID)
		response.TerminalEventID, response.WorkspaceVersionID = &id, &version
		var data struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return api.SessionTurn{}, err
		}
		response.Result, response.Error = data.Result, data.Error
	}
	return response, nil
}
