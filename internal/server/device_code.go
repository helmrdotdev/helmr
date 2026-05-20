package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

func (s *Server) startDeviceCode(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	codes, err := auth.GenerateDeviceCodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate device code"))
		return
	}
	deviceHash, err := auth.HashToken(s.authSecret, codes.DeviceCode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("hash device code"))
		return
	}
	userHash, err := auth.HashToken(s.authSecret, auth.NormalizeUserCode(codes.UserCode))
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("hash user code"))
		return
	}
	ttl := s.effectiveDeviceCodeTTL()
	pollEvery := s.effectiveDevicePollEvery()
	_, err = s.db.CreateDeviceCode(r.Context(), db.CreateDeviceCodeParams{
		ID:                  ids.ToPG(ids.New()),
		UserCodeHash:        userHash,
		DeviceCodeHash:      deviceHash,
		ExpiresAt:           pgTimeToPG(time.Now().Add(ttl)),
		PollIntervalSeconds: int32(pollEvery.Seconds()),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("create device code"))
		return
	}
	verificationURI := s.publicURL.ResolveReference(&url.URL{Path: "/auth/device"}).String()
	complete := s.publicURL.ResolveReference(&url.URL{Path: "/auth/device", RawQuery: "code=" + url.QueryEscape(codes.UserCode)}).String()
	writeJSON(w, http.StatusCreated, api.DeviceStartResponse{
		DeviceCode:              codes.DeviceCode,
		UserCode:                codes.UserCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: complete,
		ExpiresInSeconds:        int64(ttl.Seconds()),
		IntervalSeconds:         int64(pollEvery.Seconds()),
	})
}

func (s *Server) deviceStatus(w http.ResponseWriter, r *http.Request) {
	code := auth.NormalizeUserCode(r.URL.Query().Get("user_code"))
	device, ok := s.lookupDeviceCodeByUserCode(w, r, code)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.DeviceStatusResponse{
		Status:    deviceStatus(device),
		ExpiresAt: pgTime(device.ExpiresAt).Format(time.RFC3339),
	})
}

func (s *Server) approveDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.resolveDeviceCode(w, r, true)
}

func (s *Server) denyDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.resolveDeviceCode(w, r, false)
}

func (s *Server) resolveDeviceCode(w http.ResponseWriter, r *http.Request, approve bool) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	var request api.DeviceAuthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid device authorization JSON"))
		return
	}
	code := auth.NormalizeUserCode(request.UserCode)
	hash, err := auth.HashToken(s.authSecret, code)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid device code"))
		return
	}
	actor := actorFromContext(r.Context())
	if actor.Role == "" {
		writeError(w, http.StatusForbidden, errors.New("organization is required"))
		return
	}
	var device db.DeviceCode
	if approve {
		device, err = s.db.ApproveDeviceCode(r.Context(), db.ApproveDeviceCodeParams{
			OrgID:        ids.ToPG(actor.OrgID),
			UserID:       ids.ToPG(actor.UserID),
			UserCodeHash: hash,
		})
	} else {
		device, err = s.db.DenyDeviceCode(r.Context(), db.DenyDeviceCodeParams{
			OrgID:        ids.ToPG(actor.OrgID),
			UserID:       ids.ToPG(actor.UserID),
			UserCodeHash: hash,
		})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("device code not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("resolve device code"))
		return
	}
	writeJSON(w, http.StatusOK, api.DeviceStatusResponse{Status: deviceStatus(device)})
}

func (s *Server) deviceToken(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	var request api.DeviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid device token JSON"))
		return
	}
	hash, err := auth.HashToken(s.authSecret, strings.TrimSpace(request.DeviceCode))
	if err != nil {
		writeDeviceTokenError(w, "invalid_request")
		return
	}
	device, err := s.db.GetDeviceCodeForPoll(r.Context(), hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeDeviceTokenError(w, "invalid_request")
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("poll device code"))
		return
	}
	switch status := deviceStatus(device); status {
	case "pending":
		writeDeviceTokenError(w, "authorization_pending")
	case "denied":
		writeDeviceTokenError(w, "access_denied")
	case "expired":
		writeDeviceTokenError(w, "expired_token")
	case "approved":
		consumed, err := s.db.ConsumeDeviceCode(r.Context(), hash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeDeviceTokenError(w, "invalid_request")
				return
			}
			writeError(w, http.StatusInternalServerError, errors.New("consume device code"))
			return
		}
		token, err := s.issueSessionForOrg(r, s.db, consumed.DecidedByUserID, consumed.OrgID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("issue device session"))
			return
		}
		writeJSON(w, http.StatusOK, api.DeviceTokenResponse{
			AccessToken:      token,
			TokenType:        "bearer",
			ExpiresInSeconds: int64(s.effectiveSessionTTL().Seconds()),
		})
	default:
		writeDeviceTokenError(w, "invalid_request")
	}
}

func (s *Server) lookupDeviceCodeByUserCode(w http.ResponseWriter, r *http.Request, code string) (db.DeviceCode, bool) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return db.DeviceCode{}, false
	}
	hash, err := auth.HashToken(s.authSecret, code)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid device code"))
		return db.DeviceCode{}, false
	}
	device, err := s.db.GetDeviceCodeByUserCodeHash(r.Context(), hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("device code not found"))
			return db.DeviceCode{}, false
		}
		writeError(w, http.StatusInternalServerError, errors.New("load device code"))
		return db.DeviceCode{}, false
	}
	return device, true
}

func deviceStatus(device db.DeviceCode) string {
	if device.Status == db.DeviceCodeStatusPending && time.Now().After(pgTime(device.ExpiresAt)) {
		return "expired"
	}
	return string(device.Status)
}

func writeDeviceTokenError(w http.ResponseWriter, code string) {
	status := http.StatusBadRequest
	if code == "authorization_pending" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, api.DeviceTokenResponse{Error: code})
}
