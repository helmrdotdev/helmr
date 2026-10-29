package controlplane

import (
	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/identity"
	"net/http"
	"uuid"
)

func (s *Server) mountAccountRepairRoutes(r chi.Router) {
	if !s.selfHostedMode() {
		return
	}
	r.Group(func(r chi.Router) {
		r.Use(s.requireOperator)
		r.Post("/account-repairs/redeem", s.redeemAccountRepair)
		r.Post("/account-repairs/{id}/options", s.accountRepairRegisterOptions)
		r.Post("/account-repairs/{id}/complete", s.accountRepairRegisterComplete)
	})
}
func (s *Server) redeemAccountRepair(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Capability string `json:"capability"`
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
	result, err := identity.RedeemAccountRepair(r.Context(), s.tx, principalFromContext(r.Context()), input.Capability, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	var target string
	if err = s.tx.QueryRow(r.Context(), `SELECT u.display_name||' ('||COALESCE(u.primary_email,'')||', '||u.id::text||')' FROM account_repair_enrollments e JOIN users u ON u.id=e.user_id WHERE e.id=$1`, result.ID).Scan(&target); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrollment_id": result.ID, "expires_at": result.ExpiresAt, "target": target})
}
func (s *Server) accountRepairRegisterOptions(w http.ResponseWriter, r *http.Request) {
	id, binding, err := enrollmentRequest(w, r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	result, err := identity.BeginAccountRepairRegistration(r.Context(), s.tx, s.webauthn, principalFromContext(r.Context()), id, binding)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) accountRepairRegisterComplete(w http.ResponseWriter, r *http.Request) {
	id, binding, err := enrollmentRequest(w, r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	var input keyResponse
	if err = decodeRequestJSON(r, &input); err != nil {
		writeError(w, badRequest(err))
		return
	}
	challenge, err := uuid.Parse(input.ChallengeID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = identity.CompleteAccountRepairRegistration(r.Context(), s.tx, s.identity, s.webauthn, principalFromContext(r.Context()), id, challenge, binding, input.Credential, input.Label); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
