package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/identity"
	"net/http"
	"time"
	"uuid"
)

func (s *Server) emailChangeStart(w http.ResponseWriter, r *http.Request) {
	if !s.emailConfigured() {
		writeError(w, unavailable(errors.New("email delivery is unavailable")))
		return
	}
	var input struct {
		Email string `json:"new_email"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, true)
	if err != nil {
		writeError(w, err)
		return
	}
	c, err := identity.StartEmailChallenge(r.Context(), s.tx, s.identity, identity.EmailStart{Email: input.Email, Session: principalFromContext(r.Context()), BrowserHash: binding, EmailChange: true})
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.queueEmailCode(c)
	writeJSON(w, http.StatusAccepted, c)
}
func (s *Server) emailChangeResend(w http.ResponseWriter, r *http.Request) {
	s.emailChangeContinue(w, r, false)
}
func (s *Server) emailChangeVerify(w http.ResponseWriter, r *http.Request) {
	s.emailChangeContinue(w, r, true)
}
func (s *Server) emailChangeContinue(w http.ResponseWriter, r *http.Request, verify bool) {
	var input struct {
		ID   string `json:"challenge_id"`
		Code string `json:"code"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	id, err := uuid.Parse(input.ID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	binding, err := browserBinding(w, r, false)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	if err = identity.CheckEmailChangeChallenge(r.Context(), s.tx, principalFromContext(r.Context()), id, binding); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	if verify {
		result, err := identity.CompleteEmailChallenge(r.Context(), s.tx, s.identity, id, binding, input.Code, rawBrowserSession(r), "email_change")
		if err != nil {
			s.writeIdentityError(w, r, err)
			return
		}
		setSessionCookie(w, r, result.Token, 30*24*time.Hour)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	c, err := identity.ResendEmailChallenge(r.Context(), s.tx, s.identity, id, binding, "email_change")
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.queueEmailCode(c)
	writeJSON(w, http.StatusAccepted, c)
}
