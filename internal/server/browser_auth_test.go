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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func TestBootstrapStatusReturnsRequiredForFreshInstance(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{setupRequired: true}),
		WithBootstrapOwnerEmail(" owner@example.test "),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap/status", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.BootstrapStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.SetupEnabled || !response.BootstrapRequired || !response.BootstrapOwnerEmailConfigured {
		t.Fatalf("response = %+v", response)
	}
}

func TestLoginStartCreatesSetupFlowForFreshInstance(t *testing.T) {
	provider := &fakeAuthProvider{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{setupRequired: true}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithBootstrapOwnerEmail("owner@example.test"),
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

func TestLoginStartRequiresBootstrapOwnerEmailForFreshInstance(t *testing.T) {
	provider := &fakeAuthProvider{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&browserAuthStore{setupRequired: true}),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithAuthProvider(provider),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/github/start", strings.NewReader(`{"next":"/runs"}`))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.state != "" || provider.verifier != "" {
		t.Fatalf("auth provider was invoked state=%q verifier=%q", provider.state, provider.verifier)
	}
	if !strings.Contains(rec.Body.String(), `"error_kind":"bootstrap_owner_email_required"`) {
		t.Fatalf("body = %s", rec.Body.String())
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
	if store.createdSession.UserID != ids.ToPG(store.userID) || store.createdSession.OrgID != ids.ToPG(store.orgID) || len(store.createdSession.TokenHash) == 0 {
		t.Fatalf("created session = %+v", store.createdSession)
	}
	if store.upsertedIdentity.ProfileImageUrl.String != "https://avatars.example.test/octocat.png" {
		t.Fatalf("upserted identity = %+v", store.upsertedIdentity)
	}
	if !hasCookie(rec.Result().Cookies(), "helmr_session_dev") {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
}

func TestSetupCallbackCreatesOwnerAndIssuesSession(t *testing.T) {
	store := &browserAuthDBTX{}
	provider := &fakeAuthProvider{identity: authIdentity{
		Provider:      "github",
		Subject:       "123",
		DisplayName:   "octocat",
		Email:         "Owner@Example.Test",
		EmailVerified: true,
		Claims:        json.RawMessage(`{"login":"octocat"}`),
	}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDBTX(store),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithBootstrapOwnerEmail(" owner@example.test "),
		WithAuthProvider(provider),
	)

	start := httptest.NewRecorder()
	server.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/auth/github/start", bytes.NewReader([]byte(`{"next":"/runs"}`))))
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
	if !store.tx.lockAcquired || !store.tx.committed {
		t.Fatalf("tx lock=%v committed=%v", store.tx.lockAcquired, store.tx.committed)
	}
	if store.tx.ensuredMember.Role != db.OrgMemberRoleOwner || store.tx.ensuredMember.OrgID != ids.ToPG(ids.DefaultOrgID) {
		t.Fatalf("ensured member = %+v", store.tx.ensuredMember)
	}
	if store.tx.createdSession.UserID != store.tx.userID || store.tx.createdSession.OrgID != ids.ToPG(ids.DefaultOrgID) || len(store.tx.createdSession.TokenHash) == 0 {
		t.Fatalf("created session = %+v", store.tx.createdSession)
	}
	if !hasCookie(rec.Result().Cookies(), "helmr_session_dev") {
		t.Fatalf("cookies = %v", rec.Result().Cookies())
	}
}

func TestSetupCallbackRejectsWrongBootstrapOwnerEmail(t *testing.T) {
	store := &browserAuthDBTX{}
	provider := &fakeAuthProvider{identity: authIdentity{
		Provider:      "github",
		Subject:       "123",
		DisplayName:   "octocat",
		Email:         "wrong@example.test",
		EmailVerified: true,
	}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDBTX(store),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithBootstrapOwnerEmail("owner@example.test"),
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

	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.tx.ensuredMember.UserID.Valid || store.tx.createdSession.UserID.Valid {
		t.Fatalf("unexpected member/session creation member=%+v session=%+v", store.tx.ensuredMember, store.tx.createdSession)
	}
	if !strings.Contains(rec.Body.String(), `"error_kind":"bootstrap_owner_mismatch"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestSetupCallbackReportsUnverifiedBootstrapOwnerEmail(t *testing.T) {
	store := &browserAuthDBTX{}
	provider := &fakeAuthProvider{identity: authIdentity{
		Provider:       "github",
		Subject:        "123",
		DisplayName:    "octocat",
		EmailLookupErr: "github user emails endpoint returned 403 Forbidden",
	}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDBTX(store),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithBootstrapOwnerEmail("owner@example.test"),
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

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.tx.ensuredMember.UserID.Valid || store.tx.createdSession.UserID.Valid {
		t.Fatalf("unexpected member/session creation member=%+v session=%+v", store.tx.ensuredMember, store.tx.createdSession)
	}
	if !strings.Contains(rec.Body.String(), `"error_kind":"bootstrap_owner_email_unverified"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestSetupCallbackReturnsAlreadyBootstrappedWhenOwnerAppears(t *testing.T) {
	store := &browserAuthDBTX{ownerAppearsInTx: true}
	provider := &fakeAuthProvider{identity: authIdentity{
		Provider:      "github",
		Subject:       "123",
		DisplayName:   "octocat",
		Email:         "owner@example.test",
		EmailVerified: true,
	}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDBTX(store),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithBootstrapOwnerEmail("owner@example.test"),
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

	if rec.Code != http.StatusGone {
		t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error_kind":"already_bootstrapped"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestSetupStartIsNotFound(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup/start", strings.NewReader(`{"token":"token"}`))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetupPageRedirectsToLogin(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	req := httptest.NewRequest(http.MethodGet, "/setup?token=token", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("location"); got != "/login" {
		t.Fatalf("location = %q", got)
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
	setupRequired    bool
	createdSession   db.CreateSessionParams
	upsertedIdentity db.UpsertAuthIdentityParams
}

func (s *browserAuthStore) OwnerExists(context.Context, pgtype.UUID) (bool, error) {
	return !s.setupRequired, nil
}

func (s *browserAuthStore) GetLoginIdentityMember(context.Context, db.GetLoginIdentityMemberParams) (db.GetLoginIdentityMemberRow, error) {
	return db.GetLoginIdentityMemberRow{
		OrgID:  ids.ToPG(s.orgID),
		UserID: ids.ToPG(s.userID),
		Role:   db.OrgMemberRoleOwner,
	}, nil
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

type browserAuthDBTX struct {
	ownerAppearsInTx bool
	tx               *browserAuthTx
}

func (dbtx *browserAuthDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	panic("unexpected Exec outside transaction")
}

func (dbtx *browserAuthDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (dbtx *browserAuthDBTX) QueryRow(_ context.Context, sql string, args ...interface{}) pgx.Row {
	return browserAuthQueryRow(nil, false, sql, args...)
}

func (dbtx *browserAuthDBTX) Begin(context.Context) (pgx.Tx, error) {
	dbtx.tx = &browserAuthTx{ownerExists: dbtx.ownerAppearsInTx}
	return dbtx.tx, nil
}

type browserAuthTx struct {
	ownerExists    bool
	lockAcquired   bool
	committed      bool
	rolledBack     bool
	userID         pgtype.UUID
	ensuredMember  db.EnsureOrgMemberParams
	createdSession db.CreateSessionParams
}

func (tx *browserAuthTx) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected nested transaction")
}

func (tx *browserAuthTx) Commit(context.Context) error {
	tx.committed = true
	return nil
}

func (tx *browserAuthTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}

func (tx *browserAuthTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unexpected CopyFrom")
}

func (tx *browserAuthTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}

func (tx *browserAuthTx) LargeObjects() pgx.LargeObjects {
	panic("unexpected LargeObjects")
}

func (tx *browserAuthTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}

func (tx *browserAuthTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		tx.lockAcquired = true
		return pgconn.NewCommandTag("SELECT 1"), nil
	}
	panic("unexpected Exec")
}

func (tx *browserAuthTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (tx *browserAuthTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return browserAuthQueryRow(tx, tx.ownerExists, sql, args...)
}

func (tx *browserAuthTx) Conn() *pgx.Conn {
	return nil
}

func browserAuthQueryRow(tx *browserAuthTx, ownerExists bool, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT EXISTS"):
		return scanRow{values: []any{ownerExists}}
	case strings.Contains(sql, "WITH existing_identity"):
		userID := args[4].(pgtype.UUID)
		if tx != nil {
			tx.userID = userID
		}
		return scanRow{values: []any{
			userID,
			args[5].(string),
			args[6].(pgtype.Text),
			args[0].(pgtype.Text),
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
		}}
	case strings.Contains(sql, "INSERT INTO org_members"):
		if tx != nil {
			tx.ensuredMember = db.EnsureOrgMemberParams{
				OrgID:       args[0].(pgtype.UUID),
				UserID:      args[1].(pgtype.UUID),
				Role:        args[2].(db.OrgMemberRole),
				DisplayName: args[3].(pgtype.Text),
			}
			tx.ownerExists = true
		}
		return scanRow{values: []any{
			args[0].(pgtype.UUID),
			args[1].(pgtype.UUID),
			args[2].(db.OrgMemberRole),
			args[3].(pgtype.Text),
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
		}}
	case strings.Contains(sql, "INSERT INTO sessions"):
		if tx != nil {
			tx.createdSession = db.CreateSessionParams{
				ID:        args[0].(pgtype.UUID),
				OrgID:     args[1].(pgtype.UUID),
				UserID:    args[2].(pgtype.UUID),
				TokenHash: args[3].([]byte),
				ExpiresAt: args[4].(pgtype.Timestamptz),
			}
		}
		return scanRow{values: []any{
			args[0].(pgtype.UUID),
			args[1].(pgtype.UUID),
			args[2].(pgtype.UUID),
			args[3].([]byte),
			pgtype.Timestamptz{},
			pgtype.Timestamptz{},
			args[4].(pgtype.Timestamptz),
			pgtype.Timestamptz{},
		}}
	default:
		return scanRow{err: fmt.Errorf("unexpected query: %s", sql)}
	}
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
