package controlplane

import (
	"net/http"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAuditHTTPAuthenticationPermissionAndAdminRefusals(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	org, owner := f.organizationOwner(t, "audit-http")
	user := f.user(t, "Reader")
	f.member(t, org, user, "member")
	token := f.session(t, user, org)
	var count int
	countFacts := func() int {
		t.Helper()
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countFacts()
	if r := f.request(t, http.MethodGet, "/api/audit-events", "", ""); r.Code != http.StatusUnauthorized {
		t.Fatal(r.Code, r.Body.String())
	}
	if countFacts() != before {
		t.Fatal("missing credential created a security fact")
	}
	if r := f.request(t, http.MethodGet, "/api/audit-events", "malformed", ""); r.Code != http.StatusUnauthorized {
		t.Fatal(r.Code, r.Body.String())
	}
	if countFacts() != before+1 {
		t.Fatal("presented invalid credential not recorded")
	}
	if r := f.request(t, http.MethodPost, "/api/roles", token, `{"name":"forbidden","permissions":["audit_events.read"]}`); r.Code != http.StatusForbidden {
		t.Fatal(r.Code, r.Body.String())
	}
	var permission bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='role.created' AND outcome='denied' AND org_id=$1 AND actor_id=$2 AND details->>'reason'='permission_denied')`, org, user).Scan(&permission); err != nil || !permission {
		t.Fatal(permission, err)
	}
	if r := f.request(t, http.MethodGet, "/admin/api/v1/audit-events", owner, ""); r.Code != http.StatusForbidden {
		t.Fatal(r.Code, r.Body.String())
	}
	var admin bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='audit.searched' AND details->>'reason'='installation_authority_required')`).Scan(&admin); err != nil || !admin {
		t.Fatal(admin, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `ALTER TABLE audit_events ADD CONSTRAINT reject_denials CHECK(outcome<>'denied') NOT VALID`)
	if r := f.request(t, http.MethodGet, "/api/audit-events", "invalid", ""); r.Code != http.StatusServiceUnavailable {
		t.Fatal("ordinary denial hid capture failure", r.Code, r.Body.String())
	}
}
