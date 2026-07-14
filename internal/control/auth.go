package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type actorContextKey struct{}
type workerContextKey struct{}

type workerActor struct {
	WorkerInstanceID uuid.UUID
	WorkerGroupID    string
	WorkerEpoch      int64
	ClaimVersion     int64
	ProtocolVersion  string
	Roles            []string
	ResourceID       string
	State            db.WorkerInstanceState
	EpochStartedAt   time.Time
}

func (s *Server) requireActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r.Header.Get("authorization")); ok {
			actor, err := s.bearerActor(r, token)
			if err != nil {
				writeActorAuthError(w, s.log, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor)))
			return
		}
		actor, rawSession, err := s.sessionActor(r)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthenticated) {
				s.log.Error("session authentication failed", "error", err)
				writeError(w, unavailable(errors.New("authentication is unavailable")))
				return
			}
			clearSessionCookie(w, r)
			writeError(w, unauthorized(errors.New("authentication is required")))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor))
		recorder := newSessionRefreshResponseWriter(w, r, rawSession, s.effectiveSessionTTL())
		next.ServeHTTP(recorder, r)
		recorder.finish()
	})
}

func (s *Server) bearerActor(r *http.Request, token string) (auth.Actor, error) {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, auth.APIKeyPrefix) {
		actor, err := s.apiKeyActor(r, token)
		if err == nil {
			return actor, nil
		}
		if !errors.Is(err, auth.ErrUnauthenticated) {
			return auth.Actor{}, err
		}
		return s.sessionActorFromToken(r, token)
	}
	actor, err := s.sessionActorFromToken(r, token)
	if err == nil {
		return actor, nil
	}
	if !errors.Is(err, auth.ErrUnauthenticated) && s.userAuthConfigured() == nil {
		return auth.Actor{}, fmt.Errorf("session authentication: %w", err)
	}
	if s.auth == nil {
		return auth.Actor{}, auth.ErrUnauthenticated
	}
	return s.apiKeyActor(r, token)
}

func (s *Server) apiKeyActor(r *http.Request, token string) (auth.Actor, error) {
	if s.auth == nil {
		return auth.Actor{}, fmt.Errorf("api key authentication: authentication is not configured")
	}
	actor, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			return auth.Actor{}, err
		}
		return auth.Actor{}, fmt.Errorf("api key authentication: %w", err)
	}
	return actor, nil
}

func writeActorAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("authentication failed", "error", err)
		writeError(w, unavailable(errors.New("authentication is unavailable")))
		return
	}
	writeError(w, unauthorized(errors.New("authentication is required")))
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r.Header.Get("authorization")); ok {
			token = strings.TrimSpace(token)
			if strings.HasPrefix(token, auth.APIKeyPrefix) || !looksLikeSessionBearerToken(token) {
				writeError(w, unauthorized(errors.New("session authentication is required")))
				return
			}
			actor, err := s.sessionActorFromToken(r, token)
			if err != nil {
				writeError(w, unauthorized(errors.New("session authentication is required")))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor)))
			return
		}
		actor, rawSession, err := s.sessionActor(r)
		if err != nil {
			clearSessionCookie(w, r)
			writeError(w, unauthorized(errors.New("authentication is required")))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor))
		recorder := newSessionRefreshResponseWriter(w, r, rawSession, s.effectiveSessionTTL())
		next.ServeHTTP(recorder, r)
		recorder.finish()
	})
}

func looksLikeSessionBearerToken(token string) bool {
	if len(token) < 40 {
		return false
	}
	for _, r := range token {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) requireSessionPermission(permission auth.Permission, next http.Handler) http.Handler {
	return s.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := actorFromContext(r.Context())
		if actor.Role == "" {
			writeError(w, forbidden(errors.New("organization is required")))
			return
		}
		if !actor.HasPermission(permission, auth.Scope{OrgID: actor.OrgID}) {
			writeError(w, forbidden(errors.New("permission is required")))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) sessionActor(r *http.Request) (auth.Actor, string, error) {
	cookie, err := r.Cookie(sessionCookieName(r))
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return auth.Actor{}, "", auth.ErrUnauthenticated
	}
	actor, err := s.sessionActorFromToken(r, cookie.Value)
	return actor, cookie.Value, err
}

func (s *Server) sessionActorFromToken(r *http.Request, rawSession string) (auth.Actor, error) {
	rawSession = strings.TrimSpace(rawSession)
	if rawSession == "" {
		return auth.Actor{}, auth.ErrUnauthenticated
	}
	if err := s.userAuthConfigured(); err != nil {
		return auth.Actor{}, err
	}
	tokenHash, err := auth.HashToken(s.authSecret, rawSession)
	if err != nil {
		return auth.Actor{}, err
	}
	row, err := s.db.GetAuthSessionByTokenHash(r.Context(), tokenHash)
	if err != nil {
		if isNoRows(err) {
			return auth.Actor{}, auth.ErrUnauthenticated
		}
		return auth.Actor{}, err
	}
	sessionID, err := pgvalue.UUIDValue(row.ID)
	if err != nil {
		return auth.Actor{}, err
	}
	userID, err := pgvalue.UUIDValue(row.UserID)
	if err != nil {
		return auth.Actor{}, err
	}
	if err := s.db.RefreshAuthSession(r.Context(), db.RefreshAuthSessionParams{
		ID:        row.ID,
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(s.effectiveSessionTTL())),
	}); err != nil {
		return auth.Actor{}, err
	}
	actor := auth.Actor{
		UserID:    userID,
		SessionID: sessionID,
		Kind:      auth.ActorKindSession,
	}
	if row.OrgID.Valid {
		orgID, err := pgvalue.UUIDValue(row.OrgID)
		if err != nil {
			return auth.Actor{}, err
		}
		actor.OrgID = orgID
		actor.Role = auth.Role(row.Role)
	}
	return actor, nil
}

func (s *Server) requireWorker(next http.Handler) http.Handler {
	return s.requireWorkerState(false, false, next)
}

func (s *Server) requireRegisteringWorker(next http.Handler) http.Handler {
	return s.requireWorkerState(true, false, next)
}

func (s *Server) requireTerminalWorker(next http.Handler) http.Handler {
	return s.requireWorkerState(false, true, next)
}

func (s *Server) requireWorkerState(registering, terminal bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.db == nil || len(s.workerTokenSecret) == 0 {
			writeError(w, unavailable(errors.New("worker authentication is not configured")))
			return
		}
		token, ok := bearerToken(r.Header.Get("authorization"))
		if !ok {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		payload, err := auth.VerifyWorkerToken(s.workerTokenSecret, token, time.Now())
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		credentialID, err := uuid.Parse(payload.CredentialID)
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		workerInstanceID, err := uuid.Parse(payload.WorkerInstanceID)
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		params := db.AuthorizeWorkerInstanceCredentialParams{
			CredentialID:      pgvalue.UUID(credentialID),
			ClaimVersion:      payload.ClaimVersion,
			GroupClaimVersion: payload.GroupClaimVersion,
			ProtocolVersion:   payload.ProtocolVersion,
			WorkerEpoch:       pgtype.Int8{Int64: payload.WorkerEpoch, Valid: true},
		}
		var row db.AuthorizeWorkerInstanceCredentialRow
		var authorizationErr error
		if registering {
			startupRow, startupErr := s.db.AuthorizeRegisteringWorkerInstanceCredential(r.Context(), db.AuthorizeRegisteringWorkerInstanceCredentialParams(params))
			row, authorizationErr = db.AuthorizeWorkerInstanceCredentialRow(startupRow), startupErr
		} else if terminal {
			terminalRow, terminalErr := s.db.AuthorizeTerminalWorkerInstanceCredential(r.Context(), db.AuthorizeTerminalWorkerInstanceCredentialParams(params))
			row, authorizationErr = db.AuthorizeWorkerInstanceCredentialRow(terminalRow), terminalErr
		} else {
			row, authorizationErr = s.db.AuthorizeWorkerInstanceCredential(r.Context(), params)
		}
		if isNoRows(authorizationErr) {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		if authorizationErr != nil {
			s.log.Error("worker instance credential authorization failed", "worker_instance_id", payload.WorkerInstanceID, "error", authorizationErr)
			writeError(w, unavailable(errors.New("worker authentication is unavailable")))
			return
		}
		worker := workerActor{
			WorkerInstanceID: workerInstanceID,
			WorkerGroupID:    strings.TrimSpace(row.WorkerGroupID),
			ClaimVersion:     row.ClaimVersion,
			ResourceID:       strings.TrimSpace(row.ResourceID),
			WorkerEpoch:      payload.WorkerEpoch,
			ProtocolVersion:  payload.ProtocolVersion,
			Roles:            append([]string(nil), payload.Roles...),
			State:            row.WorkerState,
			EpochStartedAt:   pgvalue.Time(row.EpochStartedAt),
		}
		if pgvalue.MustUUIDValue(row.WorkerInstanceID) != workerInstanceID || worker.WorkerGroupID != payload.WorkerGroupID {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		if payload.WorkerGroupID != worker.WorkerGroupID || payload.ClaimVersion != worker.ClaimVersion {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		expectedRoles := make([]string, 0, 2)
		if row.SupportsBuild {
			expectedRoles = append(expectedRoles, auth.WorkerRoleBuild)
		}
		if row.SupportsRun {
			expectedRoles = append(expectedRoles, auth.WorkerRoleRun)
		}
		if !slices.Equal(payload.Roles, expectedRoles) {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), workerContextKey{}, worker)))
	})
}

func requireActiveWorkerRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		worker := workerFromContext(r.Context())
		if worker.State != db.WorkerInstanceStateActive || !slices.Contains(worker.Roles, role) {
			writeError(w, forbidden(errors.New("active worker role is required")))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireWorkerRole authorizes an already authenticated worker for one
// execution domain without imposing lifecycle state. In-flight work must be
// able to renew and complete while the worker is draining; only new claims use
// requireActiveWorkerRole.
func requireWorkerRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(workerFromContext(r.Context()).Roles, role) {
			writeError(w, forbidden(errors.New("worker role is required")))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func actorFromContext(ctx context.Context) auth.Actor {
	actor, _ := ctx.Value(actorContextKey{}).(auth.Actor)
	return actor
}

func workerFromContext(ctx context.Context) workerActor {
	worker, _ := ctx.Value(workerContextKey{}).(workerActor)
	return worker
}

func bearerToken(value string) (string, bool) {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

type sessionRefreshResponseWriter struct {
	http.ResponseWriter
	request     *http.Request
	rawSession  string
	ttl         time.Duration
	wroteHeader bool
}

func newSessionRefreshResponseWriter(w http.ResponseWriter, r *http.Request, rawSession string, ttl time.Duration) *sessionRefreshResponseWriter {
	return &sessionRefreshResponseWriter{
		ResponseWriter: w,
		request:        r,
		rawSession:     rawSession,
		ttl:            ttl,
	}
}

func (w *sessionRefreshResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.ensureSessionCookie()
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *sessionRefreshResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *sessionRefreshResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *sessionRefreshResponseWriter) finish() {
	if !w.wroteHeader {
		w.ensureSessionCookie()
	}
}

func (w *sessionRefreshResponseWriter) ensureSessionCookie() {
	if w.Header().Get("set-cookie") == "" {
		setSessionCookie(w.ResponseWriter, w.request, w.rawSession, w.ttl)
	}
}

func sessionCookieName(r *http.Request) string {
	if isSecureRequest(r) {
		return "__Host-helmr_session"
	}
	return "helmr_session_dev"
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, maxAge time.Duration) {
	cookie := &http.Cookie{
		Name:     sessionCookieName(r),
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
		Secure:   isSecureRequest(r),
	}
	http.SetCookie(w, cookie)
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(r),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Secure:   isSecureRequest(r),
	})
}

func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil ||
		strings.EqualFold(r.Header.Get("x-forwarded-proto"), "https") ||
		strings.EqualFold(r.Header.Get("cloudfront-forwarded-proto"), "https")
}
