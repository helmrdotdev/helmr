package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/identity"
)

func (s *Server) emailConfigured() bool {
	_, disabled := s.mailer.(email.Unconfigured)
	return s.mailer != nil && !disabled
}
func bindingCookieName(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		if isSecureRequest(r) {
			return "__Host-helmr_operator_binding"
		}
		return "helmr_operator_binding_dev"
	}
	if isSecureRequest(r) {
		return "__Host-helmr_binding"
	}
	return "helmr_binding_dev"
}
func browserBinding(w http.ResponseWriter, r *http.Request, create bool) ([]byte, error) {
	cookie, err := r.Cookie(bindingCookieName(r))
	if err == nil && len(cookie.Value) >= 40 {
		hash := sha256.Sum256([]byte(cookie.Value))
		return hash[:], nil
	}
	if !create {
		return nil, identity.ErrInvalidToken
	}
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{Name: bindingCookieName(r), Value: raw, Path: "/", HttpOnly: true, Secure: isSecureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	hash := sha256.Sum256([]byte(raw))
	return hash[:], nil
}
func rawBrowserSession(r *http.Request) string {
	if c, e := r.Cookie(sessionCookieName(r)); e == nil {
		return c.Value
	}
	return ""
}
func (s *Server) optionalAccount(r *http.Request) auth.Principal {
	raw := rawBrowserSession(r)
	if raw == "" {
		return auth.Principal{}
	}
	p, err := identity.AuthenticateLoginSession(r.Context(), s.tx, s.identity, raw, uuid.Nil())
	if err != nil || p.Channel != "browser" {
		return auth.Principal{}
	}
	return p
}
func (s *Server) emailStart(w http.ResponseWriter, r *http.Request) {
	if !s.emailConfigured() {
		writeError(w, unavailable(errors.New("email delivery is unavailable")))
		return
	}
	var request api.EmailStartRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	input := identity.EmailStart{Email: request.Email, Next: validateRedirectAfter(request.Next), BrowserHash: binding, Session: s.optionalAccount(r)}
	if request.InvitationToken != "" {
		invitation, _, err := s.resolveInvitation(r, request.InvitationToken)
		if err != nil {
			s.writeIdentityError(w, r, err)
			return
		}
		input.InvitationID = invitation.ID
	}
	challenge, err := identity.StartEmailChallenge(r.Context(), s.tx, s.identity, input)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.queueEmailCode(r, challenge)
	writeJSON(w, http.StatusAccepted, challenge)
}
func (s *Server) queueEmailCode(r *http.Request, c identity.EmailChallenge) {
	job := emailCodeDeliveryJob{id: fmt.Sprintf("%s/%d", c.ID, c.Generation), deliver: func(ctx context.Context) error {
		err := s.mailer.SendEmail(ctx, email.Message{To: c.Email, Subject: "Your Helmr verification code", PlainText: fmt.Sprintf("Your Helmr verification code is %s.\n\nEnter it in the browser where you requested it. It expires at %s.\nIf you did not request this code, ignore this message.\n", c.Code, c.ExpiresAt.UTC().Format(time.RFC3339)), IdempotencyKey: fmt.Sprintf("email-code/%s/%d", c.ID, c.Generation)})
		markErr := identity.MarkEmailDelivery(ctx, s.tx, c, err == nil)
		return errors.Join(err, markErr)
	}, fail: func(ctx context.Context) error { return identity.MarkEmailDelivery(ctx, s.tx, c, false) }}
	if s.emailCodeDelivery == nil || !s.emailCodeDelivery.enqueue(job) {
		if err := identity.MarkEmailDelivery(r.Context(), s.tx, c, false); err != nil {
			s.log.Error("mark code delivery failure", "error", err)
		}
	}
}
func (s *Server) emailResend(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	purpose, err := identity.LoginChallengePurpose(r.Context(), s.tx, id, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	c, err := identity.ResendEmailChallenge(r.Context(), s.tx, s.identity, id, binding, purpose)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.queueEmailCode(r, c)
	writeJSON(w, http.StatusAccepted, c)
}
func (s *Server) emailVerify(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.EmailVerifyRequest
	if err = decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	purpose, err := identity.LoginChallengePurpose(r.Context(), s.tx, id, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	completed, err := identity.CompleteEmailChallenge(r.Context(), s.tx, s.identity, id, binding, request.Code, rawBrowserSession(r), purpose)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	setSessionCookie(w, r, completed.Token, 30*24*time.Hour)
	writeJSON(w, http.StatusOK, api.AuthenticationFinishResponse{RedirectAfter: validateRedirectAfter(completed.Next)})
}
