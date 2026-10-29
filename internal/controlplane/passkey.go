package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/identity"
)

type keyResponse struct {
	ChallengeID string          `json:"challenge_id"`
	Credential  json.RawMessage `json:"credential"`
	Label       string          `json:"label"`
}

func (s *Server) passkeyOptions(w http.ResponseWriter, r *http.Request) {
	if s.webauthn == nil {
		writeError(w, unavailable(errors.New("passkey authentication is unavailable")))
		return
	}
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	options, err := identity.BeginPasskeyLogin(r.Context(), s.tx, s.webauthn, false, binding, s.optionalAccount(r))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}
func (s *Server) passkeyVerify(w http.ResponseWriter, r *http.Request) {
	var request keyResponse
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	id, err := uuid.Parse(request.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	raw, err := identity.FinishPasskeyLogin(r.Context(), s.tx, s.identity, s.webauthn, false, id, binding, request.Credential, rawBrowserSession(r))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	setSessionCookie(w, r, raw, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, api.AuthenticationFinishResponse{RedirectAfter: "/"})
}
func (s *Server) accountPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	options, err := identity.BeginAccountPasskey(r.Context(), s.tx, s.webauthn, principalFromContext(r.Context()), binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}
func (s *Server) accountPasskeyComplete(w http.ResponseWriter, r *http.Request) {
	var request keyResponse
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	id, err := uuid.Parse(request.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	if err = identity.FinishAccountPasskey(r.Context(), s.tx, s.identity, s.webauthn, principalFromContext(r.Context()), id, binding, request.Credential, request.Label); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) accountCredentials(w http.ResponseWriter, r *http.Request) {
	items, err := identity.AccountCredentials(r.Context(), s.tx, principalFromContext(r.Context()))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": items})
}
func (s *Server) removeCredential(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = identity.RemoveCredential(r.Context(), s.tx, s.identity, principalFromContext(r.Context()), id); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) revokeAccountAccess(w http.ResponseWriter, r *http.Request) {
	if err := identity.RevokeAccountAccess(r.Context(), s.tx, s.identity, principalFromContext(r.Context())); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	clearSessionCookie(w, r)
	clearBinding(w, r)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) updateAccountProfile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DisplayName string `json:"display_name"`
	}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := identity.UpdateAccountProfile(r.Context(), s.tx, s.identity, principalFromContext(r.Context()), strings.TrimSpace(request.DisplayName)); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func clearBinding(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: bindingCookieName(r), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: isSecureRequest(r), SameSite: http.SameSiteLaxMode})
	clearAuthFlowCookie(w, r)
}

func operatorCookieName(r *http.Request) string {
	if isSecureRequest(r) {
		return "__Host-helmr_operator"
	}
	return "helmr_operator_dev"
}
func operatorToken(r *http.Request) string {
	c, e := r.Cookie(operatorCookieName(r))
	if e != nil {
		return ""
	}
	return c.Value
}
func setOperatorCookie(w http.ResponseWriter, r *http.Request, raw string) {
	http.SetCookie(w, &http.Cookie{Name: operatorCookieName(r), Value: raw, Path: "/", MaxAge: 12 * 3600, HttpOnly: true, Secure: isSecureRequest(r), SameSite: http.SameSiteLaxMode})
}
func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Helmr-Auth-Context") != "" {
			writeError(w, forbidden(errors.New("operator cookie is required")))
			return
		}
		p, _, err := identity.AuthenticateOperator(r.Context(), s.tx, s.identity, operatorToken(r))
		if err != nil {
			if presentedCredential(r) && !s.refuseOperation(w, r, "authentication_invalid") {
				return
			}
			writeCookieAuthError(w, s.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p)))
	})
}
func (s *Server) mountOperatorRoutes(r chi.Router) {
	r.Post("/operator/enrollment/redeem", s.operatorRedeem)
	r.Post("/operator/enrollment/{id}/options", s.operatorRegisterOptions)
	r.Post("/operator/enrollment/{id}/complete", s.operatorRegisterComplete)
	r.Post("/operator/enrollment/{id}/assert/options", s.operatorAssertOptions)
	r.Post("/operator/enrollment/{id}/assert/verify", s.operatorAssertVerify)
	r.Post("/operator/auth/options", s.operatorLoginOptions)
	r.Post("/operator/auth/verify", s.operatorLoginVerify)
	r.Post("/operator/auth/logout", s.operatorLogout)
	r.With(s.requireOperator).Post("/operator/auth/renew/options", s.operatorRenewOptions)
	r.With(s.requireOperator).Post("/operator/auth/renew/verify", s.operatorRenewVerify)
	r.With(s.requireOperator).Get("/operator/credentials", s.operatorCredentials)
	r.With(s.requireOperator).Post("/operator/credentials/enroll", s.operatorKeyEnrollment)
	r.With(s.requireOperator).Delete("/operator/credentials/{id}", s.operatorRemoveKey)
	r.With(s.requireOperator).Get("/operator/me", s.operatorMe)
	r.With(s.requireOperator).Get("/operator/setup", s.operatorSetup)
}
func (s *Server) operatorRedeem(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Capability string `json:"capability"`
	}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := identity.RedeemOperatorEnrollment(r.Context(), s.tx, request.Capability, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func enrollmentRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, []byte, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return id, nil, err
	}
	binding, err := browserBinding(w, r, false)
	return id, binding, err
}
func (s *Server) operatorRegisterOptions(w http.ResponseWriter, r *http.Request) {
	id, binding, err := enrollmentRequest(w, r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	result, err := identity.BeginOperatorRegistration(r.Context(), s.tx, s.webauthn, id, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) operatorRegisterComplete(w http.ResponseWriter, r *http.Request) {
	id, binding, err := enrollmentRequest(w, r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	var request keyResponse
	if err = decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	challenge, err := uuid.Parse(request.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = identity.CompleteOperatorRegistration(r.Context(), s.tx, s.webauthn, id, challenge, binding, request.Credential); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollment_id": id, "next": "assert"})
}
func (s *Server) operatorAssertOptions(w http.ResponseWriter, r *http.Request) {
	id, binding, err := enrollmentRequest(w, r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	result, err := identity.BeginOperatorEnrollmentAssertion(r.Context(), s.tx, s.webauthn, id, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) operatorAssertVerify(w http.ResponseWriter, r *http.Request) {
	id, binding, err := enrollmentRequest(w, r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	var request keyResponse
	if err = decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	challenge, err := uuid.Parse(request.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = identity.VerifyOperatorEnrollment(r.Context(), s.tx, s.identity, s.webauthn, id, challenge, binding, request.Credential, request.Label); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) operatorLoginOptions(w http.ResponseWriter, r *http.Request) {
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	p, _, _ := identity.AuthenticateOperator(r.Context(), s.tx, s.identity, operatorToken(r))
	if p.OperatorID == uuid.Nil() {
		p = auth.Principal{}
	}
	options, err := identity.BeginPasskeyLogin(r.Context(), s.tx, s.webauthn, true, binding, p)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}
func (s *Server) operatorLoginVerify(w http.ResponseWriter, r *http.Request) {
	var request keyResponse
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	id, err := uuid.Parse(request.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	raw, err := identity.FinishPasskeyLogin(r.Context(), s.tx, s.identity, s.webauthn, true, id, binding, request.Credential, operatorToken(r))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	setOperatorCookie(w, r, raw)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) operatorMe(w http.ResponseWriter, r *http.Request) {
	p, expiry, err := identity.AuthenticateOperator(r.Context(), s.tx, s.identity, operatorToken(r))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": p.OperatorID, "expires_at": expiry})
}
func (s *Server) operatorSetup(w http.ResponseWriter, r *http.Request) {
	var required bool
	if err := s.tx.QueryRow(r.Context(), `SELECT NOT EXISTS(SELECT 1 FROM org_members WHERE is_owner AND removed_at IS NULL)`).Scan(&required); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"owner_setup_required": required})
}
func (s *Server) operatorLogout(w http.ResponseWriter, r *http.Request) {
	if err := identity.RevokeOperatorSession(r.Context(), s.tx, s.identity, operatorToken(r)); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: operatorCookieName(r), Path: "/", MaxAge: -1, HttpOnly: true, Secure: isSecureRequest(r)})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) browserMutationGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// The Slack callback only bridges untrusted protocol fields to a same-site
		// page; verification and explicit consent remain guarded mutations.
		if r.Method == http.MethodPost && r.URL.Path == "/api/slack/user-links/callback" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" && rawBrowserSession(r) == "" && operatorToken(r) == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/auth/device/start" || r.URL.Path == "/api/auth/device/token" {
			if rawBrowserSession(r) == "" && r.Header.Get("Origin") == "" {
				next.ServeHTTP(w, r)
				return
			}
		}
		if s.appOrigin == nil || r.Header.Get("Origin") != s.appOrigin.Scheme+"://"+s.appOrigin.Host {
			writeError(w, forbidden(errors.New("same-origin request is required")))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) operatorRenewOptions(w http.ResponseWriter, r *http.Request) {
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := identity.BeginOperatorRenew(r.Context(), s.tx, s.webauthn, binding, principalFromContext(r.Context()))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) operatorRenewVerify(w http.ResponseWriter, r *http.Request) {
	var input keyResponse
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	id, err := uuid.Parse(input.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	raw, err := identity.FinishOperatorRenew(r.Context(), s.tx, s.identity, s.webauthn, id, binding, input.Credential, operatorToken(r))
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	setOperatorCookie(w, r, raw)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) operatorCredentials(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	rows, err := s.tx.Query(r.Context(), `SELECT id,label,created_at,last_used_at FROM passkey_credentials WHERE operator_id=$1 AND revoked_at IS NULL ORDER BY created_at,id`, p.OperatorID)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	items := []identity.CredentialSummary{}
	for rows.Next() {
		var item identity.CredentialSummary
		item.Method = "passkey"
		if err = rows.Scan(&item.ID, &item.Label, &item.CreatedAt, &item.LastUsedAt); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": items})
}
func (s *Server) operatorKeyEnrollment(w http.ResponseWriter, r *http.Request) {
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := identity.BeginOperatorKeyEnrollment(r.Context(), s.tx, principalFromContext(r.Context()), binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) operatorRemoveKey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = identity.RemoveOperatorKey(r.Context(), s.tx, s.identity, principalFromContext(r.Context()), id); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
