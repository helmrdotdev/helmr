package control

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

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
	userID := uuid.Must(uuid.NewV7())
	store := &deviceTokenStore{
		deviceHash: deviceHash,
		device: db.DeviceCode{
			ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:           pgvalue.UUID(dbtest.DefaultOrgID),
			DeviceCodeHash:  deviceHash,
			DecidedByUserID: pgvalue.UUID(userID),
			Status:          db.DeviceCodeStatusApproved,
			ExpiresAt:       pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, AuthSecret: []byte(authSecret), PublicURL: mustParseTestURL("https://helmr.example.test"), SessionTTL: time.Hour})
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
	if store.createdSession.UserID != pgvalue.UUID(userID) {
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
		t.Fatal("session token does not match created session")
	}
}

func TestDeviceStartCreatesPendingCodeWithoutOrganization(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	store := &deviceTokenStore{}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, AuthSecret: []byte(authSecret), PublicURL: mustParseTestURL("https://helmr.example.test")})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/start", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.createdDeviceCode.UserCodeHash) == 0 || len(store.createdDeviceCode.DeviceCodeHash) == 0 {
		t.Fatalf("device hashes were not stored: %+v", store.createdDeviceCode)
	}
}

func TestBearerActorAcceptsSessionToken(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "helmr_session-token"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.Must(uuid.NewV7())
	store := &deviceTokenStore{
		sessionHash: sessionHash,
		session: db.GetAuthSessionByTokenHashRow{
			ID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:       pgvalue.UUID(dbtest.DefaultOrgID),
			UserID:      pgvalue.UUID(userID),
			Role:        string(db.OrgMemberRoleDeveloper),
			DisplayName: "CLI User",
			ExpiresAt:   pgvalue.Timestamptz(time.Now().Add(time.Hour)),
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
	if actor.Kind != auth.ActorKindSession || actor.UserID != userID || actor.OrgID != dbtest.DefaultOrgID {
		t.Fatalf("actor = %+v", actor)
	}
	if !store.refreshedSession.Valid {
		t.Fatal("session was not refreshed")
	}
}

func TestRequireActorAcceptsBearerSessionWithoutAPIKeyAuthenticator(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	store := &deviceTokenStore{
		sessionHash: sessionHash,
		session: db.GetAuthSessionByTokenHashRow{
			ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			UserID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Role:      string(db.OrgMemberRoleDeveloper),
			ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
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

func TestRequireSessionAcceptsBearerSession(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	store := &deviceTokenStore{
		sessionHash: sessionHash,
		session: db.GetAuthSessionByTokenHashRow{
			ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			UserID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Role:      string(db.OrgMemberRoleOwner),
			ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		},
	}
	server := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:         store,
		authSecret: []byte(authSecret),
		publicURL:  mustParseURL(t, "https://helmr.example.test"),
		sessionTTL: time.Hour,
	}
	handler := server.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := actorFromContext(r.Context())
		if actor.Kind != auth.ActorKindSession || actor.Role != auth.RoleOwner {
			t.Fatalf("actor = %+v", actor)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("authorization", "Bearer "+rawSession)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequireSessionRejectsAPIKeyBearer(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
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
		session: db.GetAuthSessionByTokenHashRow{
			ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			UserID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Role:      string(db.OrgMemberRoleDeveloper),
			ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
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
	deviceHash        []byte
	device            db.DeviceCode
	createdDeviceCode db.CreateDeviceCodeParams
	createdSession    db.CreateAuthSessionParams
	issuedAPIKeys     []db.IssueAPIKeyParams
	sessionHash       []byte
	session           db.GetAuthSessionByTokenHashRow
	refreshedSession  pgtype.UUID
	revokedSession    bool
}

func (s *deviceTokenStore) CreateDeviceCode(_ context.Context, arg db.CreateDeviceCodeParams) (db.DeviceCode, error) {
	s.createdDeviceCode = arg
	return db.DeviceCode{
		ID:                  arg.ID,
		UserCodeHash:        arg.UserCodeHash,
		DeviceCodeHash:      arg.DeviceCodeHash,
		Status:              db.DeviceCodeStatusPending,
		ExpiresAt:           arg.ExpiresAt,
		PollIntervalSeconds: arg.PollIntervalSeconds,
	}, nil
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

func (s *deviceTokenStore) CreateAuthSession(_ context.Context, arg db.CreateAuthSessionParams) (db.AuthSession, error) {
	s.createdSession = arg
	return db.AuthSession{
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

func (s *deviceTokenStore) GetAuthSessionByTokenHash(_ context.Context, hash []byte) (db.GetAuthSessionByTokenHashRow, error) {
	if !bytes.Equal(hash, s.sessionHash) || s.revokedSession {
		return db.GetAuthSessionByTokenHashRow{}, pgx.ErrNoRows
	}
	return s.session, nil
}

func (s *deviceTokenStore) RefreshAuthSession(_ context.Context, arg db.RefreshAuthSessionParams) error {
	s.refreshedSession = arg.ID
	return nil
}

func (s *deviceTokenStore) RevokeAuthSessionByTokenHash(_ context.Context, hash []byte) (int64, error) {
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
