package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAuthContinuationUsesEffectiveOrganization(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	publicURL, _ := url.Parse("https://helmr.example.test")
	s := &Server{tx: database.Pool, db: queries, authKeys: keys, publicURL: publicURL, deploymentMode: deploymentModeManagedCloud}
	user, orgA, orgB := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO users (id, display_name) VALUES ($1, 'Developer')", user.String())
	exec("INSERT INTO organizations (id, name, slug) VALUES ($1, 'First org', 'first'), ($2, 'Invited org', 'invited')", orgA.String(), orgB.String())
	exec("INSERT INTO org_members (org_id, user_id, role, created_at) VALUES ($1, $3, 'owner', now() - interval '1 day'), ($2, $3, 'viewer', now())", orgA.String(), orgB.String(), user.String())
	exec("INSERT INTO regions (id, display_name) VALUES ('local', 'Local')")
	exec("INSERT INTO projects (id, org_id, default_region_id, slug, name) VALUES ($1, $2, 'local', 'first', 'First')", uuid.NewV7().String(), orgA.String())
	issue := func(org pgtype.UUID, userID uuid.UUID) string {
		t.Helper()
		token, err := s.issueSessionForOrg(httptest.NewRequest("POST", "/", nil), queries, pgvalue.UUID(userID), org)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	request := func(handler http.HandlerFunc, token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.requireSession(handler).ServeHTTP(w, r)
		return w
	}
	pinned := issue(pgvalue.UUID(orgB), user)
	w := request(s.me, pinned, "GET", "/api/me", "")
	var me api.MeResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}
	if me.OrgID != orgB.String() || me.OrgName != "Invited org" || me.Role != "viewer" || !me.ProjectRequired || me.OrganizationRequired {
		t.Fatalf("me used another membership: %+v", me)
	}
	for _, permission := range me.Permissions {
		if permission == "tasks.deploy" {
			t.Fatal("advertised first org owner permission")
		}
	}

	// Ordinary unpinned login retains the existing first-membership selection.
	unpinned := issue(pgtype.UUID{}, user)
	w = request(s.me, unpinned, "GET", "/api/me", "")
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.OrgID != orgA.String() || me.Role != "owner" || me.ProjectRequired {
		t.Fatalf("ordinary login: %+v", me)
	}

	start := httptest.NewRecorder()
	s.startDeviceCode(start, httptest.NewRequest("POST", "/", nil))
	var device api.DeviceStartResponse
	if start.Code != 201 || json.Unmarshal(start.Body.Bytes(), &device) != nil {
		t.Fatalf("start: %s", start.Body.String())
	}
	consent := func(userID, orgID string) string {
		b, _ := json.Marshal(api.DeviceAuthorizeRequest{UserCode: device.UserCode, UserID: userID, OrgID: orgID})
		return string(b)
	}
	// Another tab changed org or user: the displayed context must not silently bind.
	for _, body := range []string{consent(user.String(), orgA.String()), consent(uuid.NewV7().String(), orgB.String()), consent("", "")} {
		w = request(s.approveDeviceCode, pinned, "POST", "/api/auth/device/approve", body)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "device_identity_changed") {
			t.Fatalf("mismatch: %d %s", w.Code, w.Body.String())
		}
	}
	w = request(s.approveDeviceCode, pinned, "POST", "/api/auth/device/approve", consent(user.String(), orgB.String()))
	if w.Code != 200 {
		t.Fatalf("org without project could not approve: %d %s", w.Code, w.Body.String())
	}
	hash, _ := auth.HashToken(keys.DeviceCode, auth.NormalizeUserCode(device.UserCode))
	decided, err := queries.GetDeviceCodeByUserCodeHash(t.Context(), hash)
	if err != nil || decided.OrgID != pgvalue.UUID(orgB) || decided.Status != db.DeviceCodeStatusApproved {
		t.Fatalf("approval: %+v %v", decided, err)
	}

	// Missing membership is onboarding, not authorization. Revoked pinned membership
	// instead invalidates that session: it cannot fall through to another org.
	newcomer := uuid.NewV7()
	exec("INSERT INTO users (id, display_name) VALUES ($1, 'New user')", newcomer.String())
	newToken := issue(pgtype.UUID{}, newcomer)
	w = request(s.me, newToken, "GET", "/api/me", "")
	me = api.MeResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if !me.OrganizationRequired || me.OrgID != "" {
		t.Fatalf("newcomer: %+v", me)
	}
	w = request(s.approveDeviceCode, newToken, "POST", "/api/auth/device/approve", consent(newcomer.String(), ""))
	if w.Code != 403 {
		t.Fatalf("no org approved: %d", w.Code)
	}
	exec("UPDATE org_members SET disabled_at = now() WHERE org_id = $1 AND user_id = $2", orgB.String(), user.String())
	w = request(s.me, pinned, "GET", "/api/me", "")
	if w.Code != 401 {
		t.Fatalf("revoked pinned org accepted: %d %s", w.Code, w.Body.String())
	}
	w = request(s.me, unpinned, "GET", "/api/me", "")
	if w.Code != 200 {
		t.Fatalf("active first membership lost: %d", w.Code)
	}
}

func TestAuthContinuationProvidersReturnStoredDestination(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	publicURL, _ := url.Parse("https://helmr.example.test")
	queries := db.New(database.Pool)
	s := &Server{db: queries, tx: database.Pool, authKeys: keys, publicURL: publicURL, authProvider: continuationAuthProvider{}}
	const next = "/auth/device?code=ABCD-EFGH#confirm"
	start := httptest.NewRecorder()
	payload, _ := json.Marshal(api.GitHubAuthStartRequest{Next: next})
	s.githubStart(start, httptest.NewRequest("POST", "https://helmr.example.test/api/auth/github/start", bytes.NewReader(payload)))
	var started api.GitHubAuthStartResponse
	if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &started) != nil {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	redirect, err := url.Parse(started.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(api.GitHubAuthFinishRequest{State: redirect.Query().Get("state"), Code: "fixture"})
	r := httptest.NewRequest("POST", "https://helmr.example.test/api/auth/github/finish", bytes.NewReader(payload))
	for _, cookie := range start.Result().Cookies() {
		r.AddCookie(cookie)
	}
	finish := httptest.NewRecorder()
	s.githubFinish(finish, r)
	var finished api.GitHubAuthFinishResponse
	if finish.Code != 200 || json.Unmarshal(finish.Body.Bytes(), &finished) != nil || finished.RedirectAfter != next {
		t.Fatalf("GitHub lost next: %d %s", finish.Code, finish.Body.String())
	}

	token := "magic-link-fixture-token"
	hash, err := auth.HashToken(keys.MagicLink, token)
	if err != nil {
		t.Fatal(err)
	}
	link, err := queries.CreateMagicLink(t.Context(), db.CreateMagicLinkParams{ID: pgvalue.UUID(uuid.NewV7()), Purpose: db.MagicLinkPurposeLogin, TokenHash: hash, Email: "fixture@example.test", RedirectAfter: pgtype.Text{String: next, Valid: true}, ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkMagicLinkSent(t.Context(), link.ID); err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(api.MagicLinkFinishRequest{Token: token})
	finish = httptest.NewRecorder()
	s.magicLinkFinish(finish, httptest.NewRequest("POST", "https://helmr.example.test/api/auth/magic-link/finish", bytes.NewReader(payload)))
	if finish.Code != 200 || json.Unmarshal(finish.Body.Bytes(), &finished) != nil || finished.RedirectAfter != next {
		t.Fatalf("magic link lost next: %d %s", finish.Code, finish.Body.String())
	}
}
