package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDeviceTokenIssuesSessionToken(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	deviceCode := "device-code"
	deviceHash, err := auth.HashToken([]byte(authSecret), deviceCode)
	if err != nil {
		t.Fatal(err)
	}
	userID := ids.New()
	store := &deviceTokenStore{
		deviceHash: deviceHash,
		device: db.DeviceCode{
			ID:              ids.ToPG(ids.New()),
			OrgID:           ids.ToPG(ids.DefaultOrgID),
			DeviceCodeHash:  deviceHash,
			DecidedByUserID: ids.ToPG(userID),
			Status:          db.DeviceCodeStatusApproved,
			ExpiresAt:       pgTimeToPG(time.Now().Add(time.Minute)),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth(authSecret, "https://helmr.example.test"),
		WithSessionTTL(time.Hour),
	)
	body, err := json.Marshal(api.DeviceTokenRequest{DeviceCode: deviceCode})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/token", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createdSession.UserID != ids.ToPG(userID) || store.createdSession.OrgID != ids.ToPG(ids.DefaultOrgID) {
		t.Fatalf("created session = %+v", store.createdSession)
	}
	if len(store.issuedAPIKeys) != 0 {
		t.Fatalf("device token should not issue API keys: %+v", store.issuedAPIKeys)
	}
	var response api.DeviceTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AccessToken == "" || response.TokenType != "bearer" || response.ExpiresInSeconds != int64(time.Hour.Seconds()) {
		t.Fatalf("response = %+v", response)
	}
	hash, err := auth.HashToken([]byte(authSecret), response.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, store.createdSession.TokenHash) {
		t.Fatal("response token does not match created session")
	}
}

func TestBearerActorAcceptsSessionToken(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "helmr_session-token"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	userID := ids.New()
	store := &deviceTokenStore{
		sessionHash: sessionHash,
		session: db.GetSessionByTokenHashRow{
			ID:          ids.ToPG(ids.New()),
			OrgID:       ids.ToPG(ids.DefaultOrgID),
			UserID:      ids.ToPG(userID),
			Role:        db.OrgMemberRoleDeveloper,
			DisplayName: "CLI User",
			ExpiresAt:   pgTimeToPG(time.Now().Add(time.Hour)),
		},
	}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		auth:       unauthenticator{},
		authSecret: []byte(authSecret),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
		sessionTTL: time.Hour,
	}
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)

	actor, err := server.bearerActor(req, rawSession)
	if err != nil {
		t.Fatal(err)
	}
	if actor.Kind != auth.ActorKindSession || actor.UserID != userID || actor.OrgID != ids.DefaultOrgID {
		t.Fatalf("actor = %+v", actor)
	}
	if !store.refreshedSession.Valid {
		t.Fatal("session was not refreshed")
	}
}

func TestRequireActorAcceptsBearerSessionWithoutAPIKeyAuthenticator(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "session-token"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	store := &deviceTokenStore{
		sessionHash: sessionHash,
		session: db.GetSessionByTokenHashRow{
			ID:        ids.ToPG(ids.New()),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			UserID:    ids.ToPG(ids.New()),
			Role:      db.OrgMemberRoleDeveloper,
			ExpiresAt: pgTimeToPG(time.Now().Add(time.Hour)),
		},
	}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		authSecret: []byte(authSecret),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
		sessionTTL: time.Hour,
	}
	handler := server.requireActor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := actorFromContext(r.Context())
		if actor.Kind != auth.ActorKindSession {
			t.Fatalf("actor = %+v", actor)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req.Header.Set("authorization", "Bearer "+rawSession)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLogoutRevokesBearerSession(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "session-token"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	store := &deviceTokenStore{
		sessionHash: sessionHash,
		session: db.GetSessionByTokenHashRow{
			ID:        ids.ToPG(ids.New()),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			UserID:    ids.ToPG(ids.New()),
			Role:      db.OrgMemberRoleDeveloper,
			ExpiresAt: pgTimeToPG(time.Now().Add(time.Hour)),
		},
	}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		auth:       unauthenticator{},
		authSecret: []byte(authSecret),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
		sessionTTL: time.Hour,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("authorization", "Bearer "+rawSession)
	rec := httptest.NewRecorder()

	server.logout(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.revokedSession {
		t.Fatal("session was not revoked")
	}
	_, err = server.bearerActor(httptest.NewRequest(http.MethodGet, "/api/runs", nil), rawSession)
	if err == nil {
		t.Fatal("revoked bearer session still authenticated")
	}
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("bearerActor error = %v", err)
	}
}

type unauthenticator struct{}

func (unauthenticator) Authenticate(context.Context, string) (auth.Actor, error) {
	return auth.Actor{}, auth.ErrUnauthenticated
}

type deviceTokenStore struct {
	db.Querier
	deviceHash       []byte
	device           db.DeviceCode
	createdSession   db.CreateSessionParams
	issuedAPIKeys    []db.IssueAPIKeyParams
	sessionHash      []byte
	session          db.GetSessionByTokenHashRow
	refreshedSession pgtype.UUID
	revokedSession   bool
}

func (s *deviceTokenStore) GetDeviceCodeForPoll(_ context.Context, hash []byte) (db.DeviceCode, error) {
	if !bytes.Equal(hash, s.deviceHash) {
		return db.DeviceCode{}, context.Canceled
	}
	return s.device, nil
}

func (s *deviceTokenStore) ConsumeDeviceCode(_ context.Context, hash []byte) (db.DeviceCode, error) {
	if !bytes.Equal(hash, s.deviceHash) {
		return db.DeviceCode{}, context.Canceled
	}
	return s.device, nil
}

func (s *deviceTokenStore) CreateSession(_ context.Context, arg db.CreateSessionParams) (db.Session, error) {
	s.createdSession = arg
	return db.Session{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		UserID:    arg.UserID,
		TokenHash: arg.TokenHash,
		ExpiresAt: arg.ExpiresAt,
	}, nil
}

func (s *deviceTokenStore) IssueAPIKey(_ context.Context, arg db.IssueAPIKeyParams) (db.APIKey, error) {
	s.issuedAPIKeys = append(s.issuedAPIKeys, arg)
	return db.APIKey{}, nil
}

func (s *deviceTokenStore) GetSessionByTokenHash(_ context.Context, hash []byte) (db.GetSessionByTokenHashRow, error) {
	if !bytes.Equal(hash, s.sessionHash) || s.revokedSession {
		return db.GetSessionByTokenHashRow{}, pgx.ErrNoRows
	}
	return s.session, nil
}

func (s *deviceTokenStore) RefreshSession(_ context.Context, arg db.RefreshSessionParams) error {
	s.refreshedSession = arg.ID
	return nil
}

func (s *deviceTokenStore) RevokeSessionByTokenHash(_ context.Context, hash []byte) (int64, error) {
	if !bytes.Equal(hash, s.sessionHash) || s.revokedSession {
		return 0, nil
	}
	s.revokedSession = true
	return 1, nil
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
