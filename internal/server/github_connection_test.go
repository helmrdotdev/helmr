package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"
)

func TestListGitHubInstallationsRequiresOwnerSession(t *testing.T) {
	store := &githubConnectionStore{role: db.OrgMemberRoleOwner}
	server := testGitHubConnectionServer(store, &fakeGitHubConnector{})
	req := httptest.NewRequest(http.MethodGet, "/api/github/installations", nil)
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListGitHubInstallations(t *testing.T) {
	store := &githubConnectionStore{
		role: db.OrgMemberRoleOwner,
		installations: []db.GitHubAppInstallation{
			{
				InstallationID:      123,
				AccountLogin:        "helmrdotdev",
				AccountType:         "Organization",
				RepositorySelection: pgtype.Text{String: "selected", Valid: true},
				HtmlUrl:             pgtype.Text{String: "https://github.com/settings/installations/123", Valid: true},
				CreatedAt:           testTime(),
				UpdatedAt:           testTime(),
			},
		},
	}
	server := testGitHubConnectionServer(store, &fakeGitHubConnector{installURL: "https://github.com/apps/helmr/installations/new"})
	req := httptest.NewRequest(http.MethodGet, "/api/github/installations", nil)
	addSessionCookie(req)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.GitHubInstallationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.InstallURL != "https://github.com/apps/helmr/installations/new" || len(response.Installations) != 1 {
		t.Fatalf("response = %+v", response)
	}
	item := response.Installations[0]
	if item.InstallationID != "123" || item.AccountLogin != "helmrdotdev" || item.RepositorySelection != "selected" || item.Status != "active" {
		t.Fatalf("item = %+v", item)
	}
}

func TestEnableProjectWorkspaceRepositoryAllowsAPIKeyActorWithoutUserID(t *testing.T) {
	projectID := ids.New()
	store := &githubConnectionStore{defaultProjectID: projectID}
	server := testGitHubConnectionServer(store, &fakeGitHubConnector{}, WithAuthenticator(fakeAuth{
		kind: auth.ActorKindAPIKey,
		permissions: []auth.PermissionGrant{{
			ProjectID:     auth.DefaultProjectID,
			EnvironmentID: auth.DefaultEnvironmentID,
			Permissions:   []auth.Permission{auth.PermissionProjectsManage},
		}},
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/github/workspace-repositories/enable", strings.NewReader(`{"installation_id":"123","github_repository_id":"456"}`))
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.workspaceRepositoryEnable.ProjectID != ids.ToPG(projectID) {
		t.Fatalf("project_id = %+v", store.workspaceRepositoryEnable.ProjectID)
	}
	if store.workspaceRepositoryEnable.EnabledByUserID.Valid {
		t.Fatalf("enabled_by_user_id should be null for API key actor: %+v", store.workspaceRepositoryEnable.EnabledByUserID)
	}
}

func TestGitHubSetupCallbackVerifiesAndConnectsInstallation(t *testing.T) {
	store := &githubConnectionStore{role: db.OrgMemberRoleOwner, orgID: ids.New(), userID: ids.New()}
	provider := &fakeGitHubAuthProvider{accessToken: "github-user-token"}
	connector := &fakeGitHubConnector{verified: ghapp.VerifiedInstallation{
		InstallationID:      123,
		AccountLogin:        "helmrdotdev",
		AccountType:         "Organization",
		RepositorySelection: "selected",
		HTMLURL:             "https://github.com/settings/installations/123",
	}}
	server := testGitHubConnectionServer(store, connector, WithAuthProvider(provider))

	startReq := httptest.NewRequest(http.MethodPost, "/api/github/setup/start", strings.NewReader(`{"installation_id":"123"}`))
	addSessionCookie(startReq)
	startRec := httptest.NewRecorder()
	server.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", startRec.Code, startRec.Body.String())
	}

	callbackBody, _ := json.Marshal(api.GitHubAuthFinishRequest{Code: "code", State: provider.state})
	callbackReq := httptest.NewRequest(http.MethodPost, "/api/auth/github/finish", bytes.NewReader(callbackBody))
	addSessionCookie(callbackReq)
	for _, cookie := range startRec.Result().Cookies() {
		if strings.Contains(cookie.Name, "auth_flow") {
			callbackReq.AddCookie(cookie)
		}
	}
	callbackRec := httptest.NewRecorder()
	server.ServeHTTP(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body=%s", callbackRec.Code, callbackRec.Body.String())
	}
	if connector.userToken != "github-user-token" || connector.installationID != 123 {
		t.Fatalf("connector token=%q installation=%d", connector.userToken, connector.installationID)
	}
	if store.upsert.InstallationID != 123 || store.upsert.OrgID != ids.ToPG(store.orgID) || store.upsert.AccountLogin != "helmrdotdev" {
		t.Fatalf("upsert = %+v", store.upsert)
	}
	if !store.upsert.HtmlUrl.Valid || store.upsert.HtmlUrl.String == "" {
		t.Fatalf("html url = %+v", store.upsert.HtmlUrl)
	}
	var response api.GitHubAuthFinishResponse
	if err := json.Unmarshal(callbackRec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RedirectAfter != "/settings/github" {
		t.Fatalf("response = %+v", response)
	}
}

func TestGitHubSetupCallbackRejectsInstallationConnectedToAnotherOrg(t *testing.T) {
	currentOrgID := ids.New()
	otherOrgID := ids.New()
	store := &githubConnectionStore{
		role:   db.OrgMemberRoleOwner,
		orgID:  currentOrgID,
		userID: ids.New(),
		existingInstallation: db.GitHubAppInstallation{
			OrgID:          ids.ToPG(otherOrgID),
			InstallationID: 123,
		},
	}
	provider := &fakeGitHubAuthProvider{accessToken: "github-user-token"}
	connector := &fakeGitHubConnector{verified: ghapp.VerifiedInstallation{
		InstallationID: 123,
		AccountLogin:   "helmrdotdev",
		AccountType:    "Organization",
	}}
	server := testGitHubConnectionServer(store, connector, WithAuthProvider(provider))

	startReq := httptest.NewRequest(http.MethodPost, "/api/github/setup/start", strings.NewReader(`{"installation_id":"123"}`))
	addSessionCookie(startReq)
	startRec := httptest.NewRecorder()
	server.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", startRec.Code, startRec.Body.String())
	}

	callbackBody, _ := json.Marshal(api.GitHubAuthFinishRequest{Code: "code", State: provider.state})
	callbackReq := httptest.NewRequest(http.MethodPost, "/api/auth/github/finish", bytes.NewReader(callbackBody))
	addSessionCookie(callbackReq)
	for _, cookie := range startRec.Result().Cookies() {
		if strings.Contains(cookie.Name, "auth_flow") {
			callbackReq.AddCookie(cookie)
		}
	}
	callbackRec := httptest.NewRecorder()
	server.ServeHTTP(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", callbackRec.Code, callbackRec.Body.String())
	}
	if store.upsert.InstallationID != 0 {
		t.Fatalf("unexpected upsert = %+v", store.upsert)
	}
}

func TestGitHubSetupStartRequiresOwnerSession(t *testing.T) {
	for name, tc := range map[string]struct {
		addCookie bool
		role      db.OrgMemberRole
		want      int
	}{
		"anonymous": {want: http.StatusUnauthorized},
		"developer": {addCookie: true, role: db.OrgMemberRoleDeveloper, want: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			store := &githubConnectionStore{role: tc.role}
			server := testGitHubConnectionServer(store, &fakeGitHubConnector{})
			req := httptest.NewRequest(http.MethodPost, "/api/github/setup/start", strings.NewReader(`{"installation_id":"123"}`))
			if tc.addCookie {
				addSessionCookie(req)
			}
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGitHubSetupCallbackRequiresOwnerSession(t *testing.T) {
	store := &githubConnectionStore{role: db.OrgMemberRoleOwner}
	provider := &fakeGitHubAuthProvider{accessToken: "github-user-token"}
	connector := &fakeGitHubConnector{verified: ghapp.VerifiedInstallation{
		InstallationID: 123,
		AccountLogin:   "helmrdotdev",
		AccountType:    "Organization",
	}}
	server := testGitHubConnectionServer(store, connector, WithAuthProvider(provider))

	startReq := httptest.NewRequest(http.MethodPost, "/api/github/setup/start", strings.NewReader(`{"installation_id":"123"}`))
	addSessionCookie(startReq)
	startRec := httptest.NewRecorder()
	server.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", startRec.Code, startRec.Body.String())
	}

	for name, tc := range map[string]struct {
		addCookie bool
		role      db.OrgMemberRole
		want      int
	}{
		"anonymous": {want: http.StatusUnauthorized},
		"developer": {addCookie: true, role: db.OrgMemberRoleDeveloper, want: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			store.role = tc.role
			store.upsert = db.UpsertGitHubInstallationParams{}
			body, _ := json.Marshal(api.GitHubAuthFinishRequest{Code: "code", State: provider.state})
			req := httptest.NewRequest(http.MethodPost, "/api/auth/github/finish", bytes.NewReader(body))
			if tc.addCookie {
				addSessionCookie(req)
			}
			for _, cookie := range startRec.Result().Cookies() {
				if strings.Contains(cookie.Name, "auth_flow") {
					req.AddCookie(cookie)
				}
			}
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.upsert.InstallationID != 0 {
				t.Fatalf("unexpected upsert = %+v", store.upsert)
			}
		})
	}
}

type githubConnectionStore struct {
	db.Querier
	role                      db.OrgMemberRole
	orgID                     uuid.UUID
	userID                    uuid.UUID
	defaultProjectID          uuid.UUID
	defaultEnvironmentID      uuid.UUID
	installations             []db.GitHubAppInstallation
	existingInstallation      db.GitHubAppInstallation
	upsert                    db.UpsertGitHubInstallationParams
	workspaceRepositoryEnable db.EnableProjectWorkspaceRepositoryAccessParams
}

func (s *githubConnectionStore) GetSessionByTokenHash(context.Context, []byte) (db.GetSessionByTokenHashRow, error) {
	orgID := s.orgID
	if orgID == uuid.Nil {
		orgID = ids.DefaultOrgID
	}
	userID := s.userID
	if userID == uuid.Nil {
		userID = ids.New()
	}
	role := s.role
	if role == "" {
		role = db.OrgMemberRoleOwner
	}
	return db.GetSessionByTokenHashRow{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(orgID),
		UserID:    ids.ToPG(userID),
		Role:      role,
		ExpiresAt: pgTimeToPG(time.Now().Add(time.Hour)),
	}, nil
}

func (s *githubConnectionStore) RefreshSession(context.Context, db.RefreshSessionParams) error {
	return nil
}

func (s *githubConnectionStore) ListGitHubInstallations(context.Context, pgtype.UUID) ([]db.GitHubAppInstallation, error) {
	return s.installations, nil
}

func (s *githubConnectionStore) GetDefaultProjectEnvironment(context.Context, pgtype.UUID) (db.GetDefaultProjectEnvironmentRow, error) {
	projectID := s.defaultProjectID
	if projectID == uuid.Nil {
		projectID = ids.New()
	}
	environmentID := s.defaultEnvironmentID
	if environmentID == uuid.Nil {
		environmentID = ids.New()
	}
	return db.GetDefaultProjectEnvironmentRow{
		ProjectID:     ids.ToPG(projectID),
		EnvironmentID: ids.ToPG(environmentID),
	}, nil
}

func (s *githubConnectionStore) GetKnownGitHubInstallationByInstallationID(_ context.Context, installationID int64) (db.GitHubAppInstallation, error) {
	if s.existingInstallation.InstallationID == installationID {
		return s.existingInstallation, nil
	}
	return db.GitHubAppInstallation{}, pgx.ErrNoRows
}

func (s *githubConnectionStore) GetActiveGitHubRepositoryAccessTarget(_ context.Context, arg db.GetActiveGitHubRepositoryAccessTargetParams) (db.GetActiveGitHubRepositoryAccessTargetRow, error) {
	return db.GetActiveGitHubRepositoryAccessTargetRow{
		InstallationRowID:     ids.ToPG(ids.New()),
		InstallationID:        arg.InstallationID,
		AccountLogin:          "helmrdotdev",
		AccountType:           "Organization",
		InstallationCreatedAt: testTime(),
		InstallationUpdatedAt: testTime(),
		RepositoryRowID:       ids.ToPG(ids.New()),
		GithubRepositoryID:    arg.GithubRepositoryID,
		OwnerLogin:            "helmrdotdev",
		RepositoryName:        "helmr",
		FullName:              "helmrdotdev/helmr",
		ConnectionID:          ids.ToPG(ids.New()),
		RepositoryCreatedAt:   testTime(),
		RepositoryUpdatedAt:   testTime(),
	}, nil
}

func (s *githubConnectionStore) EnableProjectWorkspaceRepositoryAccess(_ context.Context, arg db.EnableProjectWorkspaceRepositoryAccessParams) (db.ProjectWorkspaceRepository, error) {
	s.workspaceRepositoryEnable = arg
	return db.ProjectWorkspaceRepository{
		ID:                 arg.ID,
		OrgID:              arg.OrgID,
		ProjectID:          arg.ProjectID,
		GithubRepositoryID: arg.GithubRepositoryID,
		EnabledByUserID:    arg.EnabledByUserID,
		CreatedAt:          testTime(),
		UpdatedAt:          testTime(),
	}, nil
}

func (s *githubConnectionStore) UpsertGitHubInstallation(_ context.Context, arg db.UpsertGitHubInstallationParams) (db.GitHubAppInstallation, error) {
	s.upsert = arg
	return db.GitHubAppInstallation{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		InstallationID:      arg.InstallationID,
		AccountLogin:        arg.AccountLogin,
		AccountType:         arg.AccountType,
		RepositorySelection: arg.RepositorySelection,
		HtmlUrl:             arg.HtmlUrl,
	}, nil
}

type fakeGitHubConnector struct {
	installURL     string
	verified       ghapp.VerifiedInstallation
	userToken      string
	installationID int64
}

func (f *fakeGitHubConnector) InstallURL() string {
	return f.installURL
}

func (f *fakeGitHubConnector) VerifyUserInstallation(_ context.Context, userAccessToken string, installationID int64) (ghapp.VerifiedInstallation, error) {
	f.userToken = userAccessToken
	f.installationID = installationID
	return f.verified, nil
}

type fakeGitHubAuthProvider struct {
	state       string
	verifier    string
	accessToken string
}

func (p *fakeGitHubAuthProvider) RedirectURL(state string, verifier string) string {
	p.state = state
	p.verifier = verifier
	return "https://github.test/oauth?state=" + state
}

func (p *fakeGitHubAuthProvider) Resolve(context.Context, string, string) (authIdentity, error) {
	return authIdentity{Provider: "github", Subject: "123"}, nil
}

func (p *fakeGitHubAuthProvider) ResolveWithToken(context.Context, string, string) (authIdentity, *oauth2.Token, error) {
	return authIdentity{Provider: "github", Subject: "123"}, &oauth2.Token{AccessToken: p.accessToken}, nil
}

func testGitHubConnectionServer(store *githubConnectionStore, connector *fakeGitHubConnector, opts ...Option) http.Handler {
	options := []Option{
		WithDB(store),
		WithUserAuth("abcdefghijabcdefghijabcdefghij12", "https://helmr.example.test"),
		WithGitHubConnector(connector),
		WithAuthProvider(&fakeGitHubAuthProvider{accessToken: "github-user-token"}),
	}
	options = append(options, opts...)
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
}
