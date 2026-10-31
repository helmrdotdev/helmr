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
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
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
			if presentedCredential(r) && !s.refuseOperation(w, r, "authentication_invalid") {
				return
			}
			writeAuthError(w, s.log, auth.ErrUnauthenticated)
			return
		}
		principal, err := s.apiKeyPrincipal(r, token)
		if err != nil {
			if presentedCredential(r) && !s.refuseOperation(w, r, "authentication_invalid") {
				return
			}
			writeAuthError(w, s.log, err)
			return
		}
		if !s.acceptExpectedActor(w, r, principal) {
			return
		}
		if !s.admitPrincipal(w, r, principal) {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		if !s.acceptExpectedAPIKeyScope(w, r, principal) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// acceptExpectedAPIKeyScope refuses a request whose X-Helmr-Project or
// X-Helmr-Environment names a Project or Environment other than the API key's
// own. An absent header expects nothing; a repeated header is malformed.
// Refusals are recorded for the authenticated key, so r must carry p.
func (s *Server) acceptExpectedAPIKeyScope(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	projects, environments := r.Header.Values("X-Helmr-Project"), r.Header.Values("X-Helmr-Environment")
	if len(projects) > 1 || len(environments) > 1 {
		if s.refuseOperation(w, r, "invalid_request") {
			writeError(w, badRequest(errors.New("X-Helmr-Project and X-Helmr-Environment accept at most one value")))
		}
		return false
	}
	expectedProject, expectedEnvironment := "", ""
	if len(projects) == 1 {
		expectedProject = strings.TrimSpace(projects[0])
	}
	if len(environments) == 1 {
		expectedEnvironment = strings.TrimSpace(environments[0])
	}
	if expectedProject == "" && expectedEnvironment == "" {
		return true
	}
	projectID, err := uuid.Parse(p.ProjectID)
	if err != nil {
		writeError(w, fmt.Errorf("api key project id: %w", err))
		return false
	}
	environmentID, err := uuid.Parse(p.EnvironmentID)
	if err != nil {
		writeError(w, fmt.Errorf("api key environment id: %w", err))
		return false
	}
	project, err := s.db.GetProject(r.Context(), db.GetProjectParams{OrgID: pgvalue.UUID(p.OrgID), ID: pgvalue.UUID(projectID)})
	if err != nil {
		writeError(w, fmt.Errorf("load api key project: %w", err))
		return false
	}
	environment, err := s.db.GetEnvironment(r.Context(), db.GetEnvironmentParams{OrgID: pgvalue.UUID(p.OrgID), ProjectID: project.ID, ID: pgvalue.UUID(environmentID)})
	if err != nil {
		writeError(w, fmt.Errorf("load api key environment: %w", err))
		return false
	}
	if (expectedProject == "" || expectedProject == project.Slug) && (expectedEnvironment == "" || expectedEnvironment == environment.Slug) {
		return true
	}
	if !s.refuseOperation(w, r, "permission_denied") {
		return false
	}
	writeError(w, forbidden(codedError{code: "api_key_scope_mismatch", message: fmt.Sprintf("This API key belongs to Project %q, Environment %q.", project.Slug, environment.Slug)}))
	return false
}

type authErrorWriter func(http.ResponseWriter, *slog.Logger, error)

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
	if r.Header.Get("X-Helmr-Auth-Context") != "" || rawBrowserSession(r) != "" || operatorToken(r) != "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if target := r.Header.Get("X-Helmr-Organization"); target != "" {
		id, e := uuid.Parse(target)
		if e != nil || id != principal.OrgID {
			return auth.Principal{}, auth.ErrUnauthenticated
		}
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
	return s.requireOperator(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := auth.RequireRecentProof(principalFromContext(r.Context())); err != nil {
				s.writeOperationError(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
}

type accountOnlyContextKey struct{}

func (s *Server) requireAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bearerToken(r.Header.Get("Authorization")); ok {
			writeError(w, forbidden(errors.New("account cookie is required")))
			return
		}
		s.requireSessionWithErrorWriter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if principalFromContext(r.Context()).Channel != "browser" {
				writeError(w, forbidden(errors.New("ordinary account authentication is required")))
				return
			}
			next.ServeHTTP(w, r)
		}), writeCookieAuthError).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountOnlyContextKey{}, true)))
	})
}

func (s *Server) requireSessionWithErrorWriter(
	next http.Handler,
	writeAuthError authErrorWriter,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Helmr-Auth-Context") != "" {
			writeError(w, forbidden(errors.New("unsupported authentication context")))
			return
		}
		if r.Header.Get("Authorization") != "" && rawBrowserSession(r) != "" {
			writeError(w, badRequest(errors.New("mixed authentication credentials")))
			return
		}
		if token, ok := bearerToken(r.Header.Get("authorization")); ok {
			token = strings.TrimSpace(token)
			if strings.HasPrefix(token, auth.APIKeyPrefix) || !looksLikeSessionBearerToken(token) {
				if presentedCredential(r) && !s.refuseOperation(w, r, "authentication_invalid") {
					return
				}
				writeAuthError(w, s.log, auth.ErrUnauthenticated)
				return
			}
			principal, err := s.sessionPrincipalFromToken(r, token)
			if err != nil {
				if presentedCredential(r) && !s.refuseOperation(w, r, "authentication_invalid") {
					return
				}
				writeAuthError(w, s.log, err)
				return
			}
			if !s.acceptExpectedActor(w, r, principal) {
				return
			}
			if !s.admitPrincipal(w, r, principal) {
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
			return
		}
		principal, _, err := s.sessionPrincipal(r)
		if err != nil {
			if presentedCredential(r) && !s.refuseOperation(w, r, "authentication_invalid") {
				return
			}
			writeAuthError(w, s.log, err)
			return
		}
		if !s.acceptExpectedActor(w, r, principal) {
			return
		}
		if !s.admitPrincipal(w, r, principal) {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		next.ServeHTTP(w, r)
	})
}

// Cookie-only entry has no HTTP authentication scheme to challenge. The
// explicit code drives browser continuation without advertising bearer access.
func writeCookieAuthError(w http.ResponseWriter, _ *slog.Logger, err error) {
	if errors.Is(err, identity.ErrOrganizationAccessDenied) {
		writeError(w, forbidden(codedError{code: "access_denied", message: "Organization access is unavailable."}))
		return
	}
	if errors.Is(err, identity.ErrOrganizationRequired) {
		writeError(w, badRequest(codedError{code: "organization_required", message: "Choose an organization."}))
		return
	}
	writeError(w, forbidden(codedError{code: "authentication_required", message: "Sign in to continue."}))
}

func writeSessionAuthError(w http.ResponseWriter, _ *slog.Logger, err error) {
	if errors.Is(err, identity.ErrOrganizationAccessDenied) {
		writeError(w, forbidden(codedError{code: "access_denied", message: "Organization access is unavailable."}))
		return
	}
	if errors.Is(err, identity.ErrOrganizationRequired) {
		writeError(w, badRequest(codedError{code: "organization_required", message: "Choose an organization."}))
		return
	}
	writeError(w, unauthorized(errors.New("session authentication is required")))
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

func (s *Server) sessionPermission(permission auth.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return s.requireSessionPermission(permission, next) }
}

func (s *Server) requireSessionPermission(permission auth.Permission, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if principal.MemberID == uuid.Nil() {
			s.writeOperationError(w, r, forbidden(errors.New("organization is required")))
			return
		}
		scope := auth.Scope{OrgID: principal.OrgID}
		if !auth.SupportsScope(permission, auth.ScopeOrganization) {
			resolved, _, _, err := s.requestEnvironmentScopeFromRequest(r, principal)
			if err != nil {
				writeError(w, err)
				return
			}
			scope = resolved
		}
		if err := auth.Require(principal, permission, scope, errors.New("permission is required")); err != nil {
			s.writeOperationError(w, r, forbidden(err))
			return
		}
		next.ServeHTTP(w, r)
	})
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
	target := uuid.Nil()
	if account, _ := r.Context().Value(accountOnlyContextKey{}).(bool); !account {
		value := r.Header.Get("X-Helmr-Organization")
		if value != "" {
			var err error
			target, err = uuid.Parse(value)
			if err != nil || target == uuid.Nil() {
				return auth.Principal{}, identity.ErrOrganizationRequired
			}
		}
		if target == uuid.Nil() && s.selfHostedMode() {
			ids, err := s.db.ListOrganizationIDs(r.Context(), 2)
			if err != nil {
				return auth.Principal{}, err
			}
			if len(ids) == 1 {
				target = uuid.UUID(ids[0].Bytes)
			}
		}
	}
	p, err := identity.AuthenticateLoginSession(r.Context(), s.tx, s.identity, rawSession, target)
	if err != nil {
		return p, err
	}
	_, bearer := bearerToken(r.Header.Get("Authorization"))
	if (bearer && p.Channel != "cli") || (!bearer && p.Channel != "browser") {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if p.Channel == "cli" {
		// Use keeps a CLI login alive; failing to record it must not fail the request.
		if err := identity.ExtendCLISession(r.Context(), s.tx, p.SessionID); err != nil {
			s.log.Warn("extend CLI session failed", "session_id", p.SessionID, "error", err)
		}
	}
	return p, nil
}

func (s *Server) requireWorker(next http.Handler) http.Handler {
	return s.requireWorkerHost(workergroup.AuthenticateHost, next)
}

func (s *Server) requireDiagnosticWorker(next http.Handler) http.Handler {
	return s.requireWorkerHostStore(workergroup.AuthenticateHost, db.New(s.diagnosticDB), next)
}

func (s *Server) requireWorkerActivation(next http.Handler) http.Handler {
	return s.requireWorkerHost(workergroup.AuthenticateActivatingHost, next)
}

func (s *Server) requireRecoveringWorker(next http.Handler) http.Handler {
	return s.requireWorkerHost(workergroup.AuthenticateRecoveringHost, next)
}

func (s *Server) requireWorkerDrainCompletion(next http.Handler) http.Handler {
	return s.requireWorkerHost(workergroup.AuthenticateDrainCompletingHost, next)
}

func (s *Server) requireWorkerFence(next http.Handler) http.Handler {
	return s.requireWorkerHost(workergroup.AuthenticateFencingHost, next)
}

// hostAuthenticator is one of the workergroup worker host authenticators.
type hostAuthenticator func(context.Context, db.Querier, workergroup.HostAuthConfig, string, time.Time) (workergroup.HostPrincipal, error)

func (s *Server) requireWorkerHost(authenticate hostAuthenticator, next http.Handler) http.Handler {
	return s.requireWorkerHostStore(authenticate, s.db, next)
}

func (s *Server) requireWorkerHostStore(authenticate hostAuthenticator, store db.Querier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("authorization"))
		if !ok {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		worker, err := authenticate(r.Context(), store, s.hostAuth, token, time.Now())
		if errors.Is(err, workergroup.ErrUnauthenticated) {
			writeError(w, unauthorized(errors.New("worker authentication is required")))
			return
		}
		if err != nil {
			s.log.Error("worker host credential authorization failed", "error", err)
			writeError(w, unavailable(errors.New("worker authentication is unavailable")))
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
		// Server inactivity is authoritative; reads never rewrite rotated cookies.
		MaxAge: int((400 * 24 * time.Hour).Seconds()),
		Secure: isSecureRequest(r),
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

// acceptExpectedActor binds a mounted Console form to its original account.
// It is an expectation, never an authentication credential or authority grant.
func (s *Server) acceptExpectedActor(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	values := r.Header.Values("X-Helmr-Actor")
	if len(values) == 0 {
		return true
	}
	actor := p.UserID
	if p.Channel == "operator" {
		actor = p.OperatorID
	}
	if (p.Channel == "browser" || p.Channel == "operator") && len(values) == 1 && values[0] == actor.String() {
		return true
	}
	if !s.refuseOperation(w, r, "authentication_invalid") {
		return false
	}
	writeError(w, conflict(codedError{code: "account_changed", message: "Your account changed. Refresh before retrying."}))
	return false
}
