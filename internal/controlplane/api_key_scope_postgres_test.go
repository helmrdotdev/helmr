package controlplane

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestAPIKeyRejectsOnlyMismatchedExpectedScope(t *testing.T) {
	f := sessiontest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=true WHERE user_id=$1`, f.User)
	var orgID, projectID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&orgID, &projectID); err != nil {
		t.Fatal(err)
	}
	cfg := completeServerConfig(t)
	cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = db.New(f.Pool), f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scope := auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: f.Environment.String()}
	key, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, f.Principal(t), scope, testAPIKeyInput{Name: "scope check", RolePermissions: []auth.Permission{auth.PermissionSecretsRead, auth.PermissionSessionsRead}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, project, environment string
		status                     int
	}{
		{"absent", "", "", http.StatusOK},
		{"match", "project", "test", http.StatusOK},
		{"project only", "project", "", http.StatusOK},
		{"project mismatch", "other", "test", http.StatusForbidden},
		{"environment mismatch", "project", "other", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/secrets", nil)
			r.Header.Set("Authorization", "Bearer "+key.Raw)
			if tc.project != "" {
				r.Header.Set("X-Helmr-Project", tc.project)
			}
			if tc.environment != "" {
				r.Header.Set("X-Helmr-Environment", tc.environment)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if tc.status == http.StatusForbidden && (!strings.Contains(w.Body.String(), `"api_key_scope_mismatch"`) || !strings.Contains(w.Body.String(), `Project \"project\", Environment \"test\"`)) {
				t.Fatalf("mismatch body: %s", w.Body.String())
			}
		})
	}
	keyID := pgvalue.MustUUIDValue(key.Record.ID)
	for _, tc := range []struct {
		name, reason string
		headers      http.Header
		status       int
	}{
		{"mismatch", "permission_denied", http.Header{"X-Helmr-Environment": {"other"}}, http.StatusForbidden},
		{"repeated", "invalid_request", http.Header{"X-Helmr-Project": {"", "project"}}, http.StatusBadRequest},
	} {
		t.Run("audited "+tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
			r.Header = tc.headers
			r.Header.Set("Authorization", "Bearer "+key.Raw)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var recorded bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='session.content_read' AND org_id=$1 AND actor_api_key_id=$2 AND credential_kind='api_key' AND details->>'reason'=$3)`, orgID, keyID, tc.reason).Scan(&recorded); err != nil || !recorded {
				t.Fatalf("refusal not attributed to the API key: %v", err)
			}
		})
	}
}
