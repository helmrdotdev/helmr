package controlplane

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestAuditHTTPPublicAndAdminProjection(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	org, owner := f.organizationOwner(t, "audit-projection")
	var user uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1`, org).Scan(&user); err != nil {
		t.Fatal(err)
	}
	dbtest.GrantMember(t, f.pool, org, user, nil, "audit_events.read")
	host, target := uuid.NewV7(), uuid.NewV7()
	appendFact := func(action, kind string, actor audit.Evidence) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		custody := org
		if definition, _ := audit.Lookup(action); definition.Scope == audit.InstallationScope {
			custody = uuid.Nil()
		}
		err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Event{ID: id, OrgID: custody, Action: action, TargetKind: kind, TargetID: target, Evidence: actor})
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := appendFact("secret.created", "secret", audit.AssignedWorker(host, 3, "computer", target, 7))
	second := appendFact("secret.created", "secret", audit.Internal(audit.ControlPlane))
	platform := appendFact("worker_group.created", "worker_group", audit.Internal(audit.ControlPlane))
	// Simulate retained data captured before installation custody was enforced.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE audit_events SET org_id=$2 WHERE id=$1`, platform, org)
	q := url.Values{"from": {time.Now().Add(-time.Hour).Format(time.RFC3339Nano)}, "to": {time.Now().Format(time.RFC3339Nano)}, "actor_kind": {"platform_service"}, "limit": {"1"}}
	get := func(path, token string, query url.Values, status int) string {
		t.Helper()
		r := f.request(t, http.MethodGet, path+"?"+query.Encode(), token, "")
		if r.Code != status {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
		return r.Body.String()
	}
	type page struct {
		Events     []json.RawMessage `json:"events"`
		NextBefore *api.AuditBefore  `json:"next_before"`
	}
	var p page
	body := get("/api/audit-events", owner, q, 200)
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 1 || p.NextBefore == nil || !strings.Contains(string(p.Events[0]), second.String()) {
		t.Fatal(body)
	}
	for _, hidden := range []string{host.String(), "actor_worker_host_id", "actor_component", "worker_host", "control_plane", `"credential_version":3`} {
		if strings.Contains(body, hidden) {
			t.Fatalf("leaked %s: %s", hidden, body)
		}
	}
	q.Set("before_recorded_at", p.NextBefore.RecordedAt.Format(time.RFC3339Nano))
	q.Set("before_id", p.NextBefore.ID.String())
	body = get("/api/audit-events", owner, q, 200)
	p = page{}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 1 || p.NextBefore != nil || !strings.Contains(string(p.Events[0]), first.String()) {
		t.Fatal(body)
	}
	q.Del("before_recorded_at")
	q.Del("before_id")
	q.Del("actor_kind")
	q.Set("actor_id", host.String())
	body = get("/api/audit-events", owner, q, 200)
	p = page{}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 0 || p.NextBefore != nil {
		t.Fatal("hidden actor ID selected a row", body)
	}
	for _, kind := range []string{"worker_host", "component", "platform_service", "unidentified"} {
		q.Set("actor_kind", kind)
		get("/api/audit-events", owner, q, 400)
	}
	q.Del("actor_id")
	q.Del("actor_kind")
	get("/admin/api/v1/audit-events", owner, q, 403)
	get("/admin/api/v1/audit-events/actions", owner, nil, 403)
	// The same human and organization route stay projected after admin elevation.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE users SET admin=true WHERE id IN (SELECT user_id FROM org_members WHERE org_id=$1)`, org)
	for _, path := range []string{"/api/audit-events/actions", "/admin/api/v1/audit-events/actions"} {
		body := get(path, owner, nil, 200)
		var catalog api.AuditActionsResponse
		if err := json.Unmarshal([]byte(body), &catalog); err != nil {
			t.Fatal(err)
		}
		foundPlatform := false
		for _, action := range catalog.Actions {
			d, ok := audit.Lookup(action.Action)
			if !ok {
				t.Fatal(action)
			}
			if d.Audience == audit.PlatformAudience {
				foundPlatform = true
			}
		}
		if foundPlatform != strings.HasPrefix(path, "/admin/") {
			t.Fatal("catalog audience", path, body)
		}
	}
	q.Set("action", "secret.created")
	q.Add("action", "worker_group.created")
	get("/api/audit-events", owner, q, 400)
	q.Del("action")
	q.Set("event_id", first.String())
	body = get("/api/audit-events", owner, q, 200)
	if !strings.Contains(body, first.String()) || strings.Contains(body, second.String()) {
		t.Fatal(body)
	}
	q.Del("event_id")
	q.Set("actor_kind", "platform_service")
	body = get("/api/audit-events", owner, q, 200)
	if strings.Contains(body, "actor_component") {
		t.Fatal(body)
	}
	q.Set("actor_kind", "worker_host")
	q.Set("actor_id", host.String())
	q.Set("organization_id", org.String())
	body = get("/admin/api/v1/audit-events", owner, q, 200)
	if !strings.Contains(body, host.String()) || !strings.Contains(body, "credential_version") {
		t.Fatal("admin lost raw evidence", body)
	}
	// Unknown eligible details must withhold the whole public page; Admin can inspect it.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE audit_events SET details='{"new_private_field":"private-marker"}'::jsonb WHERE id=$1`, second)
	q.Del("actor_id")
	q.Del("actor_kind")
	q.Set("limit", "100")
	body = get("/api/audit-events", owner, q, 503)
	if strings.Contains(body, first.String()) || strings.Contains(body, "private-marker") {
		t.Fatal("partial page escaped", body)
	}
	body = get("/admin/api/v1/audit-events", owner, q, 200)
	if !strings.Contains(body, "private-marker") || !strings.Contains(body, platform.String()) {
		t.Fatal("admin capture unavailable", body)
	}
}
