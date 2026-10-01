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

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
)

const authFlowTTL = 10 * time.Minute

type browserAuthKind string

const (
	browserAuthGitHubInvite browserAuthKind = "github_invite"
	browserAuthGitHubLogin  browserAuthKind = "github_login"
)

type browserAuthFlow struct {
	Kind          browserAuthKind `json:"kind"`
	State         string          `json:"state"`
	Verifier      string          `json:"verifier"`
	TokenHash     string          `json:"token_hash,omitempty"`
	RedirectAfter string          `json:"redirect_after,omitempty"`
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
		writeError(w, identityError(err))
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
		State:         state,
		Verifier:      verifier,
		RedirectAfter: redirectAfter,
	}
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
		writeError(w, badRequest(err))
		return
	}
	if request.State == "" || request.State != flow.State {
		writeError(w, badRequest(errors.New("auth state mismatch")))
		return
	}
	clearAuthFlowCookie(w, r)
	if request.Error != "" {
		message := strings.TrimSpace(request.ErrorDescription)
		if message == "" {
			message = "authorization failed"
		}
		writeError(w, badRequest(errors.New(message)))
		return
	}
	if request.Code == "" {
		writeError(w, badRequest(errors.New("authorization code is required")))
		return
	}
	external, err := s.authProvider.Resolve(r.Context(), request.Code, flow.Verifier)
	if err != nil {
		s.log.Warn("auth callback failed", "error", err)
		writeError(w, badRequest(errors.New("auth callback failed")))
		return
	}
	rawSession, err := s.completeBrowserAuth(r, flow, external)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	setSessionCookie(w, r, rawSession, s.identity.Lifetimes().Session)
	writeJSON(w, http.StatusOK, api.GitHubAuthFinishResponse{RedirectAfter: validateRedirectAfter(flow.RedirectAfter)})
}

func (s *Server) completeBrowserAuth(r *http.Request, flow browserAuthFlow, external identity.ExternalIdentity) (string, error) {
	switch flow.Kind {
	case browserAuthGitHubInvite:
		tokenHash, err := decodeFlowTokenHash(flow)
		if err != nil {
			return "", err
		}
		return identity.SignInWithInvitation(r.Context(), s.tx, s.identity, tokenHash, external)
	case browserAuthGitHubLogin:
		return identity.SignIn(r.Context(), s.tx, s.identity, external)
	default:
		return "", errors.New("unknown auth flow")
	}
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
	case errors.Is(err, identity.ErrInvalidToken):
		return badRequest(codedError{code: "invalid_token", message: err.Error()})
	case errors.Is(err, identity.ErrWrongAccount):
		return badRequest(codedError{code: "wrong_account", message: err.Error()})
	case errors.Is(err, identity.ErrAlreadyMember):
		return conflict(codedError{code: "already_member", message: err.Error()})
	case errors.Is(err, identity.ErrInactiveMember):
		return conflict(codedError{code: "disabled_member", message: err.Error()})
	case errors.Is(err, identity.ErrUserNotFound):
		return unauthorized(errors.New("authentication is required"))
	case errors.Is(err, identity.ErrOrganizationRequired),
		errors.Is(err, identity.ErrAPIKeyManagementRequired):
		return forbidden(err)
	case errors.Is(err, identity.ErrDeviceCodeNotFound),
		errors.Is(err, identity.ErrAPIKeyNotFound):
		return notFound(err)
	case errors.Is(err, identity.ErrDeviceApproverChanged):
		return conflict(codedError{code: "device_identity_changed", message: "Your signed-in account or organization changed. Review the current account before approving."})
	default:
		return err
	}
}
