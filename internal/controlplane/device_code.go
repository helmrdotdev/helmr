package controlplane

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"uuid"

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
	authorization, err := identity.StartDeviceCode(r.Context(), s.tx, s.identity)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	verificationURI := s.appOrigin.ResolveReference(&url.URL{Path: "/auth/device"}).String()
	complete := s.appOrigin.ResolveReference(&url.URL{Path: "/auth/device", RawQuery: "code=" + url.QueryEscape(authorization.UserCode)}).String()
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
		s.writeIdentityError(w, r, err)
		return
	}
	p := principalFromContext(r.Context())
	if p.Channel != "browser" {
		s.writeIdentityError(w, r, identity.ErrInvalidToken)
		return
	}
	response := api.DeviceStatusResponse{Status: state.Status, ExpiresAt: state.ExpiresAt.Format(time.RFC3339)}
	if state.Status == "pending" {
		if p.MemberID == uuid.Nil() {
			writeError(w, badRequest(codedError{code: "organization_required", message: "Choose an organization before approving this device."}))
			return
		}
		view := &api.DeviceConsentView{}
		if err := s.tx.QueryRow(r.Context(), `SELECT COALESCE(m.display_name,u.display_name,u.id::text),o.name FROM org_members m JOIN users u ON u.id=m.user_id JOIN organizations o ON o.id=m.org_id WHERE m.id=$1 AND m.org_id=$2 AND m.user_id=$3`, p.MemberID, p.OrgID, p.UserID).Scan(&view.AccountName, &view.OrganizationName); err != nil {
			writeError(w, err)
			return
		}
		view.Proof, err = s.deviceConsentProof(p, r.URL.Query().Get("user_code"), state.ExpiresAt.Unix())
		if err != nil {
			writeError(w, err)
			return
		}
		response.Consent = view
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) approveDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.decideDeviceCode(w, r, identity.ApproveDeviceCode)
}

func (s *Server) denyDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.decideDeviceCode(w, r, identity.DenyDeviceCode)
}

func (s *Server) decideDeviceCode(w http.ResponseWriter, r *http.Request, decide func(ctx context.Context, q db.TxBeginner, cfg identity.Config, approver auth.Principal, userCode string) (identity.DeviceCodeState, error)) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	var request api.DeviceAuthorizeRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid device authorization JSON: %w", err))
		return
	}
	p := principalFromContext(r.Context())
	if p.Channel != "browser" || !s.validDeviceConsent(p, request.UserCode, r.Header.Get("X-Helmr-Device-Consent")) {
		s.writeIdentityError(w, r, identity.ErrDeviceApproverChanged)
		return
	}
	state, err := decide(r.Context(), s.tx, s.identity, p, request.UserCode)
	if err != nil {
		s.writeIdentityError(w, r, err)
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
	rawSession, err := identity.ExchangeDeviceCode(r.Context(), s.tx, s.identity, request.DeviceCode)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, api.DeviceTokenResponse{AccessToken: rawSession})
	case errors.Is(err, identity.ErrAuthorizationPending):
		writeJSON(w, http.StatusAccepted, api.DeviceTokenResponse{Error: "authorization_pending"})
	case errors.Is(err, identity.ErrDeviceAccessDenied):
		writeDeviceTokenError(w, "access_denied")
	case errors.Is(err, identity.ErrDeviceCodeExpired):
		writeDeviceTokenError(w, "expired_token")
	case errors.Is(err, identity.ErrInvalidDeviceCode):
		writeDeviceTokenError(w, "invalid_request")
	default:
		s.writeIdentityError(w, r, err)
	}
}

// writeDeviceTokenError writes a device access token error response.
func writeDeviceTokenError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, api.DeviceTokenResponse{Error: code})
}

// This page-held proof binds the displayed consent to its browser envelope. It is
// not a login token; the code and live browser authority are checked separately.
func (s *Server) deviceConsentProof(p auth.Principal, code string, expires int64) (string, error) {
	expiry := strconv.FormatInt(expires, 10)
	message := strings.Join([]string{"device-consent", p.SessionID.String(), p.UserID.String(), p.OrgID.String(), p.MemberID.String(), auth.NormalizeUserCode(code), expiry}, "\n")
	mac, err := auth.MAC(s.authKeys.BrowserAuth, []byte(message))
	if err != nil {
		return "", err
	}
	return expiry + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}
func (s *Server) validDeviceConsent(p auth.Principal, code, proof string) bool {
	expiry, _, ok := strings.Cut(proof, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return false
	}
	expected, err := s.deviceConsentProof(p, code, expires)
	return err == nil && hmac.Equal([]byte(proof), []byte(expected))
}
