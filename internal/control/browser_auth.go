package control

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

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/jackc/pgx/v5/pgtype"
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid github invite request JSON: %w", err)))
		return
	}
	tokenHash, err := s.validateInvitationToken(r, request.Token)
	if err != nil {
		writeAuthError(w, authStartStatus(err), err)
		return
	}
	s.writeGitHubAuthStart(w, r, browserAuthGitHubInvite, tokenHash, "")
}

func (s *Server) githubStart(w http.ResponseWriter, r *http.Request) {
	var request api.GitHubAuthStartRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid github auth request JSON: %w", err)))
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
	state, err := token.GenerateOpaque(32)
	if err != nil {
		writeError(w, errors.New("generate auth state"))
		return
	}
	verifier, err := token.GenerateOpaque(64)
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid auth callback JSON: %w", err)))
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
	flow, err := s.decodeAuthFlow(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.State == "" || request.State != flow.State {
		writeError(w, badRequest(errors.New("auth state mismatch")))
		return
	}
	if request.Code == "" {
		writeError(w, badRequest(errors.New("authorization code is required")))
		return
	}
	identity, err := s.authProvider.Resolve(r.Context(), request.Code, flow.Verifier)
	if err != nil {
		s.log.Warn("auth callback failed", "error", err)
		writeError(w, badRequest(errors.New("auth callback failed")))
		return
	}
	rawSession, err := s.completeBrowserAuth(r, flow, identity)
	if err != nil {
		writeAuthError(w, callbackStatus(err), err)
		return
	}
	setSessionCookie(w, r, rawSession, s.effectiveSessionTTL())
	writeJSON(w, http.StatusOK, api.GitHubAuthFinishResponse{RedirectAfter: validateRedirectAfter(flow.RedirectAfter)})
}

func (s *Server) completeBrowserAuth(r *http.Request, flow browserAuthFlow, identity authIdentity) (string, error) {
	switch flow.Kind {
	case browserAuthGitHubInvite:
		return s.completeInviteAuth(r, flow, identity)
	case browserAuthGitHubLogin:
		return s.completeLoginAuth(r, identity)
	default:
		return "", errors.New("unknown auth flow")
	}
}

func (s *Server) completeInviteAuth(r *http.Request, flow browserAuthFlow, identity authIdentity) (string, error) {
	if s.tx == nil {
		return "", errors.New("transactional storage is not configured")
	}
	tx, err := s.tx.Begin(r.Context())
	if err != nil {
		return "", err
	}
	defer tx.Rollback(r.Context())
	queries := db.New(tx)
	tokenHash, err := decodeFlowTokenHash(flow)
	if err != nil {
		return "", err
	}
	invite, err := queries.GetActiveInvitation(r.Context(), tokenHash)
	if err != nil {
		if isNoRows(err) {
			return "", errInvalidOrExpiredToken
		}
		return "", err
	}
	if !identityMatchesInvitationEmail(identity, invite.InviteeEmail) {
		return "", errWrongAccount
	}
	user, err := s.upsertAuthIdentity(r, queries, identity)
	if err != nil {
		return "", err
	}
	if user.DisabledAt.Valid {
		return "", errDisabledMember
	}
	existingMember, err := queries.GetOrgMemberForManagement(r.Context(), db.GetOrgMemberForManagementParams{
		OrgID:  invite.OrgID,
		UserID: user.ID,
	})
	if err != nil && !isNoRows(err) {
		return "", err
	}
	if err == nil && !existingMember.DisabledAt.Valid {
		if existingMember.UserDisabledAt.Valid {
			return "", errDisabledMember
		}
		return "", errAlreadyMember
	}
	if rows, err := queries.AcceptInvitation(r.Context(), db.AcceptInvitationParams{
		OrgID:  invite.OrgID,
		ID:     invite.ID,
		UserID: user.ID,
	}); err != nil {
		return "", err
	} else if rows == 0 {
		return "", errInvalidOrExpiredToken
	}
	if _, err := queries.RevokeAuthSessionsForUser(r.Context(), user.ID); err != nil {
		return "", err
	}
	if _, err := queries.EnsureOrgMember(r.Context(), db.EnsureOrgMemberParams{
		OrgID:       invite.OrgID,
		UserID:      user.ID,
		Role:        invite.Role,
		DisplayName: pgtype.Text{String: identity.DisplayName, Valid: identity.DisplayName != ""},
	}); err != nil {
		return "", err
	}
	rawSession, err := s.issueSessionForOrg(r, queries, user.ID, invite.OrgID)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return "", err
	}
	return rawSession, nil
}

func (s *Server) completeLoginAuth(r *http.Request, identity authIdentity) (string, error) {
	user, err := s.upsertAuthIdentity(r, s.db, identity)
	if err != nil {
		return "", err
	}
	if user.DisabledAt.Valid {
		return "", errDisabledMember
	}
	return s.issueSession(r, s.db, user.ID)
}

func (s *Server) upsertAuthIdentity(r *http.Request, queries db.Querier, identity authIdentity) (db.UpsertAuthIdentityRow, error) {
	email := pgtype.Text{}
	if identity.Email != "" {
		email = pgtype.Text{String: identity.Email, Valid: true}
	}
	claims := identity.Claims
	if len(claims) == 0 || !json.Valid(claims) {
		claims = []byte(`{}`)
	}
	return queries.UpsertAuthIdentity(r.Context(), db.UpsertAuthIdentityParams{
		UserID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		IdentityID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		IdentityProvider: identity.Provider,
		IdentitySubject:  identity.Subject,
		DisplayName:      identity.DisplayName,
		ProfileImageUrl:  pgtype.Text{String: identity.ProfileImageURL, Valid: identity.ProfileImageURL != ""},
		Email:            email,
		EmailVerified:    identity.EmailVerified,
		Claims:           claims,
	})
}

func (s *Server) issueSession(r *http.Request, queries db.Querier, userID pgtype.UUID) (string, error) {
	return s.issueSessionForOrg(r, queries, userID, pgtype.UUID{})
}

func (s *Server) issueSessionForOrg(r *http.Request, queries db.Querier, userID pgtype.UUID, orgID pgtype.UUID) (string, error) {
	raw, err := token.GenerateOpaque(32)
	if err != nil {
		return "", err
	}
	hash, err := auth.HashToken(s.authSecret, raw)
	if err != nil {
		return "", err
	}
	_, err = queries.CreateAuthSession(r.Context(), db.CreateAuthSessionParams{
		ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:     orgID,
		UserID:    userID,
		TokenHash: hash,
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(s.effectiveSessionTTL())),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Server) validateInvitationToken(r *http.Request, raw string) ([]byte, error) {
	if err := s.userAuthConfigured(); err != nil {
		return nil, err
	}
	tokenHash, err := auth.HashToken(s.authSecret, raw)
	if err != nil {
		return nil, errors.New("invalid invite token")
	}
	if _, err := s.db.GetActiveInvitation(r.Context(), tokenHash); err != nil {
		if isNoRows(err) {
			return nil, errInvalidOrExpiredToken
		}
		return nil, err
	}
	return tokenHash, nil
}

func identityMatchesInvitationEmail(identity authIdentity, inviteeEmail string) bool {
	inviteeEmail = normalizeEmailAddress(inviteeEmail)
	if inviteeEmail == "" {
		return false
	}
	if identity.EmailVerified && normalizeEmailAddress(identity.Email) == inviteeEmail {
		return true
	}
	for _, email := range identity.VerifiedEmails {
		if normalizeEmailAddress(email) == inviteeEmail {
			return true
		}
	}
	return false
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
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write([]byte(encodedPayload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write([]byte(payload))
	if !hmac.Equal(actual, mac.Sum(nil)) {
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

func authStartStatus(err error) int {
	switch {
	case errors.Is(err, errInvalidOrExpiredToken):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func callbackStatus(err error) int {
	switch {
	case errors.Is(err, errInvalidOrExpiredToken), errors.Is(err, errWrongAccount):
		return http.StatusBadRequest
	case errors.Is(err, errUnknownAccount):
		return http.StatusUnauthorized
	case errors.Is(err, errAlreadyMember), errors.Is(err, errDisabledMember):
		return http.StatusConflict
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, errOwnerAccessRequired):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

func writeAuthError(w http.ResponseWriter, status int, err error) {
	if kind := authErrorKind(err); kind != "" {
		writeJSON(w, status, map[string]string{"error": err.Error(), "error_kind": kind})
		return
	}
	writeErrorStatus(w, status, err)
}

func authErrorKind(err error) string {
	switch {
	case errors.Is(err, errInvalidOrExpiredToken):
		return "invalid_token"
	case errors.Is(err, errWrongAccount):
		return "wrong_account"
	case errors.Is(err, errUnknownAccount):
		return "no_account"
	case errors.Is(err, errAlreadyMember):
		return "already_member"
	case errors.Is(err, errDisabledMember):
		return "disabled_member"
	case errors.Is(err, auth.ErrUnauthenticated):
		return "unauthenticated"
	default:
		return ""
	}
}

var (
	errInvalidOrExpiredToken = errors.New("token is invalid or expired")
	errWrongAccount          = errors.New("verified email does not match invitation")
	errUnknownAccount        = errors.New("no account exists for this identity")
	errAlreadyMember         = errors.New("identity is already a member of this organization")
	errDisabledMember        = errors.New("membership is no longer active")
	errOwnerAccessRequired   = errors.New("owner access is required")
)
