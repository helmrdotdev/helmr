package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestOrganizationCreationAdminOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		adminOnly bool
		role      string
		want      int
	}{
		{name: "ordinary signup", want: http.StatusCreated},
		{name: "unaffiliated user", adminOnly: true, want: http.StatusForbidden},
		{name: "organization owner", adminOnly: true, role: "owner", want: http.StatusForbidden},
		{name: "organization access administrator", adminOnly: true, role: "admin", want: http.StatusForbidden},
		{name: "ordinary member", adminOnly: true, role: "member", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
				cfg.DeploymentMode = deploymentModeManagedCloud
				cfg.OrganizationCreationAdminOnly = tc.adminOnly
			})
			user := fixture.user(t, "User")
			orgID, _ := fixture.organizationOwner(t, "existing")
			if tc.role != "" {
				fixture.member(t, orgID, user, tc.role)
			}
			token := fixture.session(t, user, uuid.Nil())
			me := fixture.request(t, http.MethodGet, "/api/me", token, "")
			var account api.MeResponse
			if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &account) != nil {
				t.Fatalf("me: %d %s", me.Code, me.Body.String())
			}
			if account.UserID != user.String() {
				t.Fatal("wrong account")
			}
			config := fixture.request(t, http.MethodGet, "/api/auth/config", "", "")
			var policy api.AuthConfigResponse
			if config.Code != 200 || json.Unmarshal(config.Body.Bytes(), &policy) != nil || policy.OrganizationCreationEnabled == tc.adminOnly {
				t.Fatalf("creation policy: %d %s", config.Code, config.Body)
			}

			w := fixture.request(t, http.MethodPost, "/api/organizations", token, `{"slug":"new","name":"New"}`)
			if w.Code != tc.want {
				t.Fatalf("create: %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			var count int
			if err := fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM organizations").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if tc.want == http.StatusForbidden {
				if count != 1 {
					t.Fatalf("denied creation wrote organization: count=%d", count)
				}
				return
			}
			var organization api.OrganizationSummary
			if count != 2 || json.Unmarshal(w.Body.Bytes(), &organization) != nil {
				t.Fatalf("created organization: count=%d body=%s", count, w.Body.String())
			}
			var isOwner bool
			if err := fixture.pool.QueryRow(t.Context(), "SELECT is_owner FROM org_members WHERE org_id = $1 AND user_id = $2", organization.ID, user).Scan(&isOwner); err != nil || !isOwner {
				t.Fatalf("created owner: is_owner=%t error=%v", isOwner, err)
			}
		})
	}
}

func TestOrganizationCreationRequiresIndependentOperatorForSetup(t *testing.T) {
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.DeploymentMode = deploymentModeSelfHosted
		cfg.OrganizationCreationAdminOnly = true
	})
	user := fixture.user(t, "First owner")
	token := fixture.session(t, user, uuid.Nil())
	if w := fixture.request(t, http.MethodPost, "/api/organizations", token, `{"slug":"first","name":"First"}`); w.Code != 403 {
		t.Fatalf("missing operator: %d %s", w.Code, w.Body)
	}
	operator := fixture.operatorSession(t)
	create := func(slug string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/organizations", strings.NewReader(`{"slug":"`+slug+`","name":"Organization"}`))
		r.Header.Set("Origin", fixture.origin)
		r.AddCookie(&http.Cookie{Name: sessionCookieName(r), Value: token})
		r.AddCookie(&http.Cookie{Name: operatorCookieName(r), Value: operator})
		w := httptest.NewRecorder()
		fixture.handler.ServeHTTP(w, r)
		return w
	}
	if w := create("first"); w.Code != 201 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	if w := create("second"); w.Code != 403 {
		t.Fatalf("second organization: %d %s", w.Code, w.Body)
	}
}

// operatorSession supplies an already authenticated key for HTTP admission tests.
// WebAuthn ceremony verification is exercised separately.
func (f httpPostgresFixture) operatorSession(t *testing.T) string {
	t.Helper()
	op, key, session := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashToken(f.keys.Session, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO platform_operators(id,label,webauthn_user_handle) VALUES($1,'Operator',uuid_send($1::uuid))`, []any{op}},
		{`INSERT INTO passkey_credentials(id,operator_id,credential_id,label,credential_data) VALUES($1,$2,uuid_send($1::uuid),'Security key','{}')`, []any{key, op}},
		{`INSERT INTO login_sessions(id,operator_id,channel,passkey_id,issued_at,authenticated_at,token_hash,expires_at) VALUES($1,$2,'operator',$3,now(),now(),$4,now()+interval '12 hours')`, []any{session, op, key, hash}},
	} {
		if _, err := f.pool.Exec(t.Context(), q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	f.operatorTokens[raw] = true
	return raw
}
