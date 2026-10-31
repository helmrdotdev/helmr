package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
)

const authFlowTTL = 10 * time.Minute

type browserAuthKind string

const (
	browserAuthGitHubInvite browserAuthKind = "github_invite"
	browserAuthGitHubLogin  browserAuthKind = "github_login"
	browserAuthGitHubLink   browserAuthKind = "github_link"
)

type browserAuthFlow struct {
	// SessionID is the live browser session that started the flow, or nil when
	// the browser was signed out. It survives a confirmation mid-flow, which
	// rotates the session token but keeps the session.
	SessionID uuid.UUID `json:"session_id"`

	SlackManagementSource *slackManagementSource `json:"slack_management_source,omitempty"`
	SlackConsentID        string                 `json:"slack_consent_id,omitempty"`
	StartedAt             time.Time              `json:"started_at"`
	ConnectionID          string                 `json:"connection_id,omitempty"`
	AppRegistrationID     string                 `json:"app_registration_id,omitempty"`
	EnvironmentID         string                 `json:"environment_id,omitempty"`
	SlackLinkToken        string                 `json:"slack_link_token,omitempty"`
	SlackReturnURL        string                 `json:"slack_return_url,omitempty"`
	Nonce                 string                 `json:"nonce,omitempty"`
	SlackTeamID           string                 `json:"slack_team_id,omitempty"`
	SlackUserID           string                 `json:"slack_user_id,omitempty"`
	OrganizationID        string                 `json:"org_id,omitempty"`
	UserID                string                 `json:"user_id,omitempty"`
	InstallationID        string                 `json:"installation_id,omitempty"`
	Kind                  browserAuthKind        `json:"kind"`
	State                 string                 `json:"state"`
	Verifier              string                 `json:"verifier"`
	TokenHash             string                 `json:"token_hash,omitempty"`
	RedirectAfter         string                 `json:"redirect_after,omitempty"`
}

type browserAuthEnvelope struct {
	ExpiresAt time.Time       `json:"expires_at"`
	Flow      browserAuthFlow `json:"flow"`
}

func (s *Server) githubInviteStart(w http.ResponseWriter, r *http.Request) {
	var request api.GitHubAuthInviteStartRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid github invite request JSON: %w", err))
		return
	}
	_, tokenHash, err := s.resolveInvitation(r, request.Token)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.writeGitHubAuthStart(w, r, browserAuthGitHubInvite, tokenHash, "")
}

func (s *Server) githubStart(w http.ResponseWriter, r *http.Request) {
	var request api.GitHubAuthStartRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid github auth request JSON: %w", err))
		return
	}
	s.writeGitHubAuthStart(w, r, browserAuthGitHubLogin, nil, validateRedirectAfter(request.Next))
}

func (s *Server) writeGitHubAuthStart(w http.ResponseWriter, r *http.Request, kind browserAuthKind, tokenHash []byte, redirectAfter string) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	if s.authProvider == nil {
		writeError(w, unavailable(errors.New("auth provider is not configured")))
		return
	}
	state, err := auth.GenerateOpaque(32)
	if err != nil {
		writeError(w, errors.New("generate auth state"))
		return
	}
	verifier, err := auth.GenerateOpaque(64)
	if err != nil {
		writeError(w, errors.New("generate pkce verifier"))
		return
	}
	flow := browserAuthFlow{
		Kind:          kind,
		StartedAt:     time.Now(),
		State:         state,
		Verifier:      verifier,
		RedirectAfter: redirectAfter,
	}
	session, err := s.browserSessionID(r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	flow.SessionID = session
	if len(tokenHash) > 0 {
		flow.TokenHash = base64.RawURLEncoding.EncodeToString(tokenHash)
	}
	encoded, err := s.encodeAuthFlow(flow)
	if err != nil {
		writeError(w, errors.New("encode auth flow"))
		return
	}
	http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	w.Header().Set("referrer-policy", "no-referrer")
	writeJSON(w, http.StatusOK, api.GitHubAuthStartResponse{RedirectURL: s.authProvider.RedirectURL(state, verifier)})
}

func (s *Server) githubFinish(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	if s.authProvider == nil {
		writeError(w, unavailable(errors.New("auth provider is not configured")))
		return
	}
	var request api.GitHubAuthFinishRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid auth callback JSON: %w", err))
		return
	}
	flow, err := s.decodeAuthFlow(r)
	if err != nil {
		if !s.refuseOperation(w, r, "authentication_invalid") {
			return
		}
		writeError(w, badRequest(err))
		return
	}
	if (flow.Kind != browserAuthGitHubLogin && flow.Kind != browserAuthGitHubInvite && flow.Kind != browserAuthGitHubLink) || request.State == "" || request.State != flow.State {
		if !s.refuseOperation(w, r, "authentication_invalid") {
			return
		}
		writeError(w, badRequest(errors.New("auth state mismatch")))
		return
	}
	clearAuthFlowCookie(w, r)
	if request.Error != "" {
		if !s.refuseOperation(w, r, "authentication_invalid") {
			return
		}
		message := strings.TrimSpace(request.ErrorDescription)
		if message == "" {
			message = "authorization failed"
		}
		writeError(w, badRequest(errors.New(message)))
		return
	}
	if request.Code == "" {
		if !s.refuseOperation(w, r, "authentication_invalid") {
			return
		}
		writeError(w, badRequest(errors.New("authorization code is required")))
		return
	}
	external, err := s.authProvider.Resolve(r.Context(), request.Code, flow.Verifier)
	if err != nil {
		if !s.refuseOperation(w, r, "authentication_invalid") {
			return
		}
		s.log.Warn("auth callback failed")
		writeError(w, badRequest(errors.New("auth callback failed")))
		return
	}
	rawSession, err := s.completeBrowserAuth(r, flow, external)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	setSessionCookie(w, r, rawSession, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, api.GitHubAuthFinishResponse{RedirectAfter: validateRedirectAfter(flow.RedirectAfter)})
}

// browserSessionID names the live browser session the request presents, or nil
// when the browser is signed out or its session has ended.
func (s *Server) browserSessionID(r *http.Request) (uuid.UUID, error) {
	raw := rawBrowserSession(r)
	if raw == "" {
		return uuid.Nil(), nil
	}
	p, err := identity.AuthenticateLoginSession(r.Context(), s.tx, s.identity, raw, uuid.Nil())
	if errors.Is(err, auth.ErrUnauthenticated) {
		return uuid.Nil(), nil
	}
	if err != nil {
		return uuid.Nil(), err
	}
	if p.Channel != "browser" {
		return uuid.Nil(), nil
	}
	return p.SessionID, nil
}

func (s *Server) completeBrowserAuth(r *http.Request, flow browserAuthFlow, external identity.ExternalIdentity) (string, error) {
	raw := rawBrowserSession(r)
	if flow.Kind == browserAuthGitHubLink {
		p := s.optionalAccount(r)
		if flow.SessionID == uuid.Nil() || p.SessionID != flow.SessionID {
			return "", identity.ErrWrongAccount
		}
		return identity.LinkProvider(r.Context(), s.tx, s.identity, p, external, raw)
	}
	if flow.Kind == browserAuthGitHubInvite {
		tokenHash, err := decodeFlowTokenHash(flow)
		if err != nil {
			return "", err
		}
		invitation, err := org.PendingInvitationByTokenHash(r.Context(), s.db, tokenHash)
		if err != nil {
			return "", err
		}
		if !external.Verifies(invitation.InviteeEmail) {
			return "", identity.ErrWrongAccount
		}
	}
	return identity.SignInBrowserProvider(r.Context(), s.tx, s.identity, external, raw, flow.SessionID)
}

// resolveInvitation resolves a raw invitation token for starting an
// invitation sign-in.
func (s *Server) resolveInvitation(r *http.Request, rawToken string) (org.PendingInvitation, []byte, error) {
	if err := s.userAuthConfigured(); err != nil {
		return org.PendingInvitation{}, nil, err
	}
	return identity.ResolveInvitation(r.Context(), s.db, s.identity, rawToken)
}

func decodeFlowTokenHash(flow browserAuthFlow) ([]byte, error) {
	if flow.TokenHash == "" {
		return nil, errors.New("auth flow token is missing")
	}
	tokenHash, err := base64.RawURLEncoding.DecodeString(flow.TokenHash)
	if err != nil || len(tokenHash) != sha256.Size {
		return nil, errors.New("auth flow token is invalid")
	}
	return tokenHash, nil
}

func (s *Server) encodeAuthFlow(flow browserAuthFlow) (string, error) {
	envelope := browserAuthEnvelope{ExpiresAt: time.Now().Add(authFlowTTL), Flow: flow}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac, err := auth.MAC(s.authKeys.BrowserAuth, []byte(encodedPayload))
	if err != nil {
		return "", err
	}
	signature := base64.RawURLEncoding.EncodeToString(mac)
	return encodedPayload + "." + signature, nil
}

func (s *Server) decodeAuthFlow(r *http.Request) (browserAuthFlow, error) {
	cookie, err := r.Cookie(authFlowCookieName(r))
	if err != nil {
		return browserAuthFlow{}, errors.New("auth flow has expired")
	}
	payload, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return browserAuthFlow{}, errors.New("auth flow is invalid")
	}
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return browserAuthFlow{}, errors.New("auth flow is invalid")
	}
	expected, err := auth.MAC(s.authKeys.BrowserAuth, []byte(payload))
	if err != nil || !hmac.Equal(actual, expected) {
		return browserAuthFlow{}, errors.New("auth flow is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return browserAuthFlow{}, errors.New("auth flow is invalid")
	}
	var envelope browserAuthEnvelope
	if err := json.Unmarshal(decoded, &envelope); err != nil {
		return browserAuthFlow{}, errors.New("auth flow is invalid")
	}
	if time.Now().After(envelope.ExpiresAt) {
		return browserAuthFlow{}, errors.New("auth flow has expired")
	}
	return envelope.Flow, nil
}

func validateRedirectAfter(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 || value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") || strings.Contains(value, "\x00") {
		return "/"
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	return value
}

func authFlowCookieName(r *http.Request) string {
	if isSecureRequest(r) {
		return "__Host-helmr_auth_flow"
	}
	return "helmr_auth_flow_dev"
}

func authFlowCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     authFlowCookieName(r),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Secure:   isSecureRequest(r),
	}
}

func clearAuthFlowCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, authFlowCookie(r, "", -1))
}

// identityError maps errors of the identity owner to HTTP errors. Sign-in
// failures carry a code the console distinguishes.
func identityError(err error) error {
	var input identity.InputError
	switch {
	case errors.As(err, &input),
		errors.Is(err, identity.ErrInvalidDeviceCode):
		return badRequest(err)
	case errors.Is(err, identity.ErrRecentProofRequired):
		return forbidden(codedError{code: "recent_authentication_required", message: "Sign in again to confirm it's you, then retry."})
	case errors.Is(err, identity.ErrIdentityConflict):
		return conflict(codedError{code: "identity_conflict", message: err.Error()})
	case errors.Is(err, identity.ErrAccountProofRequired):
		return forbidden(codedError{code: "account_authentication_required", message: err.Error()})
	case errors.Is(err, identity.ErrRateLimited):
		return tooManyRequests(err)
	case errors.Is(err, identity.ErrInvalidToken):
		return badRequest(codedError{code: "invalid_token", message: err.Error()})
	case errors.Is(err, identity.ErrWrongAccount):
		return badRequest(codedError{code: "wrong_account", message: err.Error()})
	case errors.Is(err, identity.ErrAlreadyMember):
		return conflict(codedError{code: "already_member", message: err.Error()})
	case errors.Is(err, identity.ErrUserNotFound):
		return forbidden(codedError{code: "authentication_required", message: "Sign in to continue."})
	case errors.Is(err, identity.ErrOrganizationRequired),
		errors.Is(err, identity.ErrAPIKeyManagementRequired):
		return forbidden(err)
	case errors.Is(err, identity.ErrDeviceCodeNotFound),
		errors.Is(err, identity.ErrAPIKeyNotFound):
		return notFound(err)
	case errors.Is(err, identity.ErrDeviceApproverChanged):
		return conflict(codedError{code: "device_identity_changed", message: "Your signed-in account or organization changed. Review the current account before approving."})
	case errors.Is(err, identity.ErrLastSignInMethod):
		return conflict(codedError{code: "last_sign_in_method", message: "Add another sign-in method before removing this one."})
	default:
		return err
	}
}

// Identity owns its failure facts. This boundary reports capture failure without
// generating a second refusal for an already recorded identity outcome.
func (s *Server) writeIdentityError(w http.ResponseWriter, r *http.Request, err error) {
	if authorityLost(err) || errors.Is(err, audit.ErrCapture) {
		s.writeOperationError(w, r, err)
		return
	}
	writeError(w, identityError(err))
}

func (s *Server) githubLinkStart(w http.ResponseWriter, r *http.Request) {
	// The account route just admitted this live browser session, so its sign-in
	// time is current; linking completes only after recent proof is rechecked.
	if err := auth.RequireRecentProof(principalFromContext(r.Context())); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.writeGitHubAuthStart(w, r, browserAuthGitHubLink, nil, "/settings/security")
}
