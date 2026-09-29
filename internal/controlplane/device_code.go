package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
)

func (s *Server) startDeviceCode(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	authorization, err := identity.StartDeviceCode(r.Context(), s.db, s.identity)
	if err != nil {
		writeError(w, err)
		return
	}
	verificationURI := s.publicURL.ResolveReference(&url.URL{Path: "/auth/device"}).String()
	complete := s.publicURL.ResolveReference(&url.URL{Path: "/auth/device", RawQuery: "code=" + url.QueryEscape(authorization.UserCode)}).String()
	writeJSON(w, http.StatusCreated, api.DeviceStartResponse{
		DeviceCode:              authorization.DeviceCode,
		UserCode:                authorization.UserCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: complete,
		ExpiresInSeconds:        int64(authorization.ExpiresIn.Seconds()),
		IntervalSeconds:         int64(authorization.PollInterval.Seconds()),
	})
}

func (s *Server) deviceStatus(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	state, err := identity.DeviceCodeStatus(r.Context(), s.db, s.identity, r.URL.Query().Get("user_code"))
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.DeviceStatusResponse{
		Status:    state.Status,
		ExpiresAt: state.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) approveDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.decideDeviceCode(w, r, identity.ApproveDeviceCode)
}

func (s *Server) denyDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.decideDeviceCode(w, r, identity.DenyDeviceCode)
}

func (s *Server) decideDeviceCode(w http.ResponseWriter, r *http.Request, decide func(ctx context.Context, q db.Querier, cfg identity.Config, approver auth.Actor, consent identity.DeviceConsent, userCode string) (identity.DeviceCodeState, error)) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	var request api.DeviceAuthorizeRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid device authorization JSON: %w", err))
		return
	}
	consent := identity.DeviceConsent{UserID: request.UserID, OrgID: request.OrgID}
	state, err := decide(r.Context(), s.db, s.identity, actorFromContext(r.Context()), consent, request.UserCode)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.DeviceStatusResponse{Status: state.Status})
}

func (s *Server) deviceToken(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	var request api.DeviceTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid device token JSON: %w", err))
		return
	}
	rawSession, err := identity.ExchangeDeviceCode(r.Context(), s.db, s.identity, request.DeviceCode)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, api.DeviceTokenResponse{
			AccessToken:      rawSession,
			TokenType:        "bearer",
			ExpiresInSeconds: int64(s.identity.Lifetimes().Session.Seconds()),
		})
	case errors.Is(err, identity.ErrAuthorizationPending):
		writeJSON(w, http.StatusAccepted, api.DeviceTokenResponse{Error: "authorization_pending"})
	case errors.Is(err, identity.ErrDeviceAccessDenied):
		writeDeviceTokenError(w, "access_denied")
	case errors.Is(err, identity.ErrDeviceCodeExpired):
		writeDeviceTokenError(w, "expired_token")
	case errors.Is(err, identity.ErrInvalidDeviceCode):
		writeDeviceTokenError(w, "invalid_request")
	default:
		writeError(w, err)
	}
}

// writeDeviceTokenError writes a device access token error response.
func writeDeviceTokenError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, api.DeviceTokenResponse{Error: code})
}
