package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLoginStartCreatesFlowCookie(t *testing.T) {
	provider := &fakeAuthProvider{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithAuthProvider(provider),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/github/start", strings.NewReader(`{"next":"/runs"}`))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.state == "" || provider.verifier == "" {
		t.Fatalf("auth provider state=%q verifier=%q", provider.state, provider.verifier)
	}
	if cookie := rec.Result().Cookies()[0]; cookie.Name != "helmr_auth_flow_dev" || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v", cookie)
	}
	var response api.GitHubAuthStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RedirectURL != "https://github.test/oauth?state="+provider.state {
		t.Fatalf("response = %+v", response)
	}
}

func TestLoginStartTrustsCloudFrontViewerProto(t *testing.T) {
	provider := &fakeAuthProvider{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://d123.cloudfront.net"),
		WithAuthProvider(provider),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/github/start", strings.NewReader(`{}`))
	req.Header.Set("cloudfront-forwarded-proto", "https")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	cookie := rec.Result().Cookies()[0]
	if cookie.Name != "__Host-helmr_auth_flow" || !cookie.Secure {
		t.Fatalf("cookie = %+v", cookie)
	}
}

func TestLoginStartCreatesLoginFlowForFreshInstance(t *testing.T) {
	provider := &fakeAuthProvider{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithAuthProvider(provider),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/github/start", strings.NewReader(`{"next":"/runs"}`))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.state == "" || provider.verifier == "" {
		t.Fatalf("auth provider state=%q verifier=%q", provider.state, provider.verifier)
	}
	if !hasCookie(rec.Result().Cookies(), "helmr_auth_flow_dev") {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
}

func TestLoginStartCreatesLoginFlowWithoutOrganization(t *testing.T) {
	provider := &fakeAuthProvider{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithAuthProvider(provider),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/github/start", strings.NewReader(`{"next":"/runs"}`))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.state == "" || provider.verifier == "" {
		t.Fatalf("auth provider state=%q verifier=%q", provider.state, provider.verifier)
	}
}

func TestLoginCallbackIssuesSession(t *testing.T) {
	store := &browserAuthStore{orgID: ids.New(), userID: ids.New()}
	provider := &fakeAuthProvider{identity: authIdentity{
		Provider:        "github",
		Subject:         "123",
		DisplayName:     "octocat",
		ProfileImageURL: "https://avatars.example.test/octocat.png",
		Claims:          json.RawMessage(`{"login":"octocat"}`),
	}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithAuthProvider(provider),
	)

	start := httptest.NewRecorder()
	server.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/auth/github/start", bytes.NewReader([]byte(`{}`))))
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", start.Code, start.Body.String())
	}

	body, _ := json.Marshal(api.GitHubAuthFinishRequest{Code: "code", State: provider.state})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/github/finish", bytes.NewReader(body))
	for _, cookie := range start.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.createdSession.UserID != ids.ToPG(store.userID) || len(store.createdSession.TokenHash) == 0 {
		t.Fatalf("created session = %+v", store.createdSession)
	}
	if store.upsertedIdentity.ProfileImageUrl.String != "https://avatars.example.test/octocat.png" {
		t.Fatalf("upserted identity = %+v", store.upsertedIdentity)
	}
	if !hasCookie(rec.Result().Cookies(), "helmr_session_dev") {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
}

type fakeAuthProvider struct {
	state    string
	verifier string
	identity authIdentity
}

func (p *fakeAuthProvider) RedirectURL(state string, verifier string) string {
	p.state = state
	p.verifier = verifier
	return "https://github.test/oauth?state=" + state
}

func (p *fakeAuthProvider) Resolve(context.Context, string, string) (authIdentity, error) {
	return p.identity, nil
}

type browserAuthStore struct {
	db.Querier
	orgID            uuid.UUID
	userID           uuid.UUID
	createdSession   db.CreateSessionParams
	upsertedIdentity db.UpsertAuthIdentityParams
}

func (s *browserAuthStore) UpsertAuthIdentity(_ context.Context, arg db.UpsertAuthIdentityParams) (db.UpsertAuthIdentityRow, error) {
	s.upsertedIdentity = arg
	return db.UpsertAuthIdentityRow{
		ID:              ids.ToPG(s.userID),
		DisplayName:     arg.DisplayName,
		ProfileImageUrl: arg.ProfileImageUrl,
		PrimaryEmail:    arg.Email,
	}, nil
}

func (s *browserAuthStore) CreateSession(_ context.Context, arg db.CreateSessionParams) (db.Session, error) {
	s.createdSession = arg
	return db.Session{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		UserID:    arg.UserID,
		TokenHash: arg.TokenHash,
		ExpiresAt: arg.ExpiresAt,
	}, nil
}

type scanRow struct {
	values []any
	err    error
}

func (row scanRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(dest) != len(row.values) {
		return fmt.Errorf("scan destinations = %d, want %d", len(dest), len(row.values))
	}
	for i, value := range row.values {
		switch target := dest[i].(type) {
		case *bool:
			*target = value.(bool)
		case *int64:
			*target = value.(int64)
		case *string:
			*target = value.(string)
		case *[]byte:
			*target = value.([]byte)
		case *pgtype.UUID:
			*target = value.(pgtype.UUID)
		case *pgtype.Text:
			*target = value.(pgtype.Text)
		case *pgtype.Timestamptz:
			*target = value.(pgtype.Timestamptz)
		case *db.OrgMemberRole:
			*target = value.(db.OrgMemberRole)
		case *db.MagicLinkPurpose:
			*target = value.(db.MagicLinkPurpose)
		default:
			return fmt.Errorf("unexpected scan target %T", target)
		}
	}
	return nil
}

func hasCookie(cookies []*http.Cookie, name string) bool {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return true
		}
	}
	return false
}
