package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

type principalContextKey struct{}
type workerContextKey struct{}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return s.requireAPIKeyWithErrorWriter(next, writePrincipalAuthError)
}

func (s *Server) requireAPIKeyWithErrorWriter(
	next http.Handler,
	writeAuthError authErrorWriter,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("authorization"))
		if !ok || !strings.HasPrefix(strings.TrimSpace(token), auth.APIKeyPrefix) {
			writeAuthError(w, s.log, auth.ErrUnauthenticated)
			return
		}
		principal, err := s.apiKeyPrincipal(r, token)
		if err != nil {
			writeAuthError(w, s.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
}

type authErrorWriter func(http.ResponseWriter, *slog.Logger, error)

func (s *Server) requirePrincipalWithErrorWriter(
	next http.Handler,
	writeAuthError authErrorWriter,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r.Header.Get("authorization")); ok {
			principal, err := s.bearerPrincipal(r, token)
			if err != nil {
				writeAuthError(w, s.log, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
			return
		}
		principal, rawSession, err := s.sessionPrincipal(r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				clearSessionCookie(w, r)
			}
			writeAuthError(w, s.log, err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		recorder := newSessionRefreshResponseWriter(w, r, rawSession, s.identity.Lifetimes().Session)
		next.ServeHTTP(recorder, r)
		recorder.finish()
	})
}

func (s *Server) bearerPrincipal(r *http.Request, token string) (auth.Principal, error) {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, auth.APIKeyPrefix) {
		principal, err := s.apiKeyPrincipal(r, token)
		if err == nil {
			return principal, nil
		}
		if !errors.Is(err, auth.ErrUnauthenticated) {
			return auth.Principal{}, err
		}
		return s.sessionPrincipalFromToken(r, token)
	}
	principal, err := s.sessionPrincipalFromToken(r, token)
	if err == nil {
		return principal, nil
	}
	if !errors.Is(err, auth.ErrUnauthenticated) && s.userAuthConfigured() == nil {
		return auth.Principal{}, fmt.Errorf("session authentication: %w", err)
	}
	if s.auth == nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return s.apiKeyPrincipal(r, token)
}

func (s *Server) apiKeyPrincipal(r *http.Request, token string) (auth.Principal, error) {
	if s.auth == nil {
		return auth.Principal{}, fmt.Errorf("api key authentication: authentication is not configured")
	}
	principal, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			return auth.Principal{}, err
		}
		return auth.Principal{}, fmt.Errorf("api key authentication: %w", err)
	}
	return principal, nil
}

func writePrincipalAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("authentication failed", "error", err)
		writeError(w, unavailable(errors.New("authentication is unavailable")))
		return
	}
	writeError(w, unauthorized(errors.New("authentication is required")))
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return s.requireSessionWithErrorWriter(next, writeSessionAuthError)
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principalFromContext(r.Context()).Admin {
			writeError(w, forbidden(errors.New("administrator access is required")))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) requireSessionWithErrorWriter(
	next http.Handler,
	writeAuthError authErrorWriter,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r.Header.Get("authorization")); ok {
			token = strings.TrimSpace(token)
			if strings.HasPrefix(token, auth.APIKeyPrefix) || !looksLikeSessionBearerToken(token) {
				writeAuthError(w, s.log, auth.ErrUnauthenticated)
				return
			}
			principal, err := s.sessionPrincipalFromToken(r, token)
			if err != nil {
				writeAuthError(w, s.log, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
			return
		}
		principal, rawSession, err := s.sessionPrincipal(r)
		if err != nil {
			clearSessionCookie(w, r)
			writeAuthError(w, s.log, err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		recorder := newSessionRefreshResponseWriter(w, r, rawSession, s.identity.Lifetimes().Session)
		next.ServeHTTP(recorder, r)
		recorder.finish()
	})
}

func writeSessionAuthError(w http.ResponseWriter, _ *slog.Logger, _ error) {
	writeError(w, unauthorized(errors.New("session authentication is required")))
}

func writeActorStartAuthError(w http.ResponseWriter, log *slog.Logger, err error) {
	if !errors.Is(err, auth.ErrUnauthenticated) {
		log.Error("Actor start authentication failed", "error", err)
		writeError(w, unavailable(codedError{
			code:      "actor_start_authority_unavailable",
			message:   "actor start authentication is unavailable",
			retryable: true,
		}))
		return
	}
	writeError(w, unauthorized(codedError{
		code:    "authentication_required",
		message: "authentication is required",
	}))
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
		principal := principalFromContext(r.Context())
		if principal.Role == "" {
			writeError(w, forbidden(errors.New("organization is required")))
			return
		}
		if !principal.HasPermission(permission, auth.Scope{OrgID: principal.OrgID}) {
			writeError(w, forbidden(errors.New("permission is required")))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) sessionPrincipal(r *http.Request) (auth.Principal, string, error) {
	cookie, err := r.Cookie(sessionCookieName(r))
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return auth.Principal{}, "", auth.ErrUnauthenticated
	}
	principal, err := s.sessionPrincipalFromToken(r, cookie.Value)
	return principal, cookie.Value, err
}

func (s *Server) sessionPrincipalFromToken(r *http.Request, rawSession string) (auth.Principal, error) {
	if strings.TrimSpace(rawSession) == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err := s.userAuthConfigured(); err != nil {
		return auth.Principal{}, err
	}
	return identity.AuthenticateLoginSession(r.Context(), s.db, s.identity, rawSession)
}

func (s *Server) requireWorker(next http.Handler) http.Handler {
	return s.requireWorkerStatus(workerAuthActive, next)
}

func (s *Server) requireWorkerActivation(next http.Handler) http.Handler {
	return s.requireWorkerStatus(workerAuthActivation, next)
}

func (s *Server) requireRecoveringWorker(next http.Handler) http.Handler {
	return s.requireWorkerStatus(workerAuthRecovering, next)
}

func (s *Server) requireWorkerDrainCompletion(next http.Handler) http.Handler {
	return s.requireWorkerStatus(workerAuthDrainCompletion, next)
}

func (s *Server) requireWorkerFence(next http.Handler) http.Handler {
	return s.requireWorkerStatus(workerAuthFence, next)
}

type workerAuthState uint8

const (
	workerAuthActive workerAuthState = iota
	workerAuthActivation
	workerAuthRecovering
	workerAuthDrainCompletion
	workerAuthFence
)

func (s *Server) requireWorkerStatus(state workerAuthState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.workerTokenSigningKey) == 0 {
			writeError(w, unavailable(errors.New("worker authentication is not configured")))
			return
		}
		token, ok := bearerToken(r.Header.Get("authorization"))
		if !ok {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		payload, err := auth.VerifyWorkerToken(s.workerTokenSigningKey, token, time.Now())
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		credentialID, err := uuid.Parse(payload.CredentialID)
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		workerHostID, err := uuid.Parse(payload.WorkerHostID)
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		workerGroupID, err := ids.Parse(payload.WorkerGroupID)
		if err != nil {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		params := db.AuthorizeWorkerHostCredentialParams{
			CredentialID:      pgvalue.UUID(credentialID),
			ClaimVersion:      payload.ClaimVersion,
			GroupClaimVersion: payload.GroupClaimVersion,
			WorkerEpoch:       pgtype.Int8{Int64: payload.WorkerEpoch, Valid: true},
		}
		var row db.AuthorizeWorkerHostCredentialRow
		var authorizationErr error
		switch state {
		case workerAuthActivation:
			activationRow, activationErr := s.db.AuthorizeWorkerActivationCredential(r.Context(), db.AuthorizeWorkerActivationCredentialParams(params))
			row, authorizationErr = db.AuthorizeWorkerHostCredentialRow(activationRow), activationErr
		case workerAuthRecovering:
			recoveryRow, recoveryErr := s.db.AuthorizeRecoveringWorkerHostCredential(r.Context(), db.AuthorizeRecoveringWorkerHostCredentialParams(params))
			row, authorizationErr = db.AuthorizeWorkerHostCredentialRow(recoveryRow), recoveryErr
		case workerAuthDrainCompletion:
			row, authorizationErr = s.db.AuthorizeWorkerHostCredential(r.Context(), params)
			if isNoRows(authorizationErr) {
				replayRow, replayErr := s.db.AuthorizeWorkerDrainReplay(r.Context(), db.AuthorizeWorkerDrainReplayParams{
					CredentialID: params.CredentialID, ClaimVersion: params.ClaimVersion,
					WorkerEpoch: params.WorkerEpoch,
				})
				row, authorizationErr = db.AuthorizeWorkerHostCredentialRow(replayRow), replayErr
			}
		case workerAuthFence:
			row, authorizationErr = s.db.AuthorizeWorkerHostCredential(r.Context(), params)
			if isNoRows(authorizationErr) {
				replayRow, replayErr := s.db.AuthorizeWorkerFenceReplay(r.Context(), db.AuthorizeWorkerFenceReplayParams{
					CredentialID: params.CredentialID, ClaimVersion: params.ClaimVersion,
					WorkerEpoch: params.WorkerEpoch,
				})
				row, authorizationErr = db.AuthorizeWorkerHostCredentialRow(replayRow), replayErr
			}
		default:
			row, authorizationErr = s.db.AuthorizeWorkerHostCredential(r.Context(), params)
		}
		if isNoRows(authorizationErr) {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		if authorizationErr != nil {
			s.log.Error("worker instance credential authorization failed", "worker_host_id", payload.WorkerHostID, "error", authorizationErr)
			writeError(w, unavailable(errors.New("worker authentication is unavailable")))
			return
		}
		worker := workergroup.HostPrincipal{
			HostID:            workerHostID,
			GroupID:           pgvalue.MustUUIDValue(row.WorkerGroupID),
			HostClaimVersion:  row.ClaimVersion,
			GroupClaimVersion: payload.GroupClaimVersion,
			ResourceID:        strings.TrimSpace(row.ResourceID),
			Epoch:             payload.WorkerEpoch,
			Status:            row.WorkerStatus,
			EpochStartedAt:    pgvalue.Time(row.EpochStartedAt),
		}
		if pgvalue.MustUUIDValue(row.WorkerHostID) != workerHostID || worker.GroupID != workerGroupID || payload.ClaimVersion != worker.HostClaimVersion {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), workerContextKey{}, worker)))
	})
}

func principalFromContext(ctx context.Context) auth.Principal {
	principal, _ := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal
}

func workerFromContext(ctx context.Context) workergroup.HostPrincipal {
	worker, _ := ctx.Value(workerContextKey{}).(workergroup.HostPrincipal)
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

func writeStaleWorkerClaims(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, workergroup.ErrStaleClaims) {
		return false
	}
	writeError(w, unauthorized(errors.New("worker authentication is required")))
	return true
}
