package audit_test

import (
	"errors"
	"fmt"
	auditquery "github.com/helmrdotdev/helmr/internal/audit/query"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
)

func TestSearchScopesReauthorizationExpiryAndAtomicAdmission(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	org, project, user, member, env, other, key := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,slug,name) VALUES($1,'audit','Audit')`, org)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name,primary_email,admin) VALUES($1,'Auditor','auditor@example.test',true)`, user)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, member, org, user)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO regions(id,display_name) VALUES('audit','Audit')`)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($1,$2,'audit','audit','Audit')`, project, org)
	for i, e := range []uuid.UUID{env, other} {
		dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($1,$2,$3,$4,'Audit','#315FCE',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, e, org, project, []string{"one", "two"}[i])
	}
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	eventIDs := []uuid.UUID{}
	for _, e := range []uuid.UUID{env, other, uuid.Nil()} {
		id := uuid.NewV7()
		eventIDs = append(eventIDs, id)
		err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, policy, audit.Event{ID: id, Evidence: audit.Internal(audit.ControlPlane), OrgID: org, EnvironmentID: e, Action: "secret.created", TargetKind: "secret", TargetID: uuid.NewV7()})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	f := auditquery.SearchFilter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), Actions: []string{"secret.created"}, Limit: 1}
	p := auth.Principal{Kind: auth.PrincipalKindSession, OrgID: org, UserID: user, MemberID: member, SessionID: uuid.NewV7(), Admin: true}
	if _, err := auditquery.Search(t.Context(), d.Pool, policy, p, f); !errors.Is(err, auditquery.ErrSearchDenied) {
		t.Fatalf("admin flag granted tenant read: %v", err)
	}
	_, denied := auditquery.Search(t.Context(), d.Pool, policy, p, f)
	var permissionRefusal *auth.PermissionRefusal
	if !errors.As(denied, &permissionRefusal) || len(permissionRefusal.Checks) != 1 || permissionRefusal.Checks[0].Permission != auth.PermissionAuditEventsRead || permissionRefusal.Checks[0].Result != "failed" {
		t.Fatal("search denial lost permission check", denied)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE users SET admin=false WHERE id=$1`, user)
	_, denied = auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, f)
	var adminRefusal *audit.Refusal
	if !errors.As(denied, &adminRefusal) || adminRefusal.Authorization.Decision != "denied" || len(adminRefusal.Authorization.Checks) != 2 {
		t.Fatal("search denial lost admin observations", denied)
	}
	observed, ok := adminRefusal.Authorization.Checks[1].(audit.UserAttributeCheck)
	if !ok || observed.Observed || observed.ObservationSource != "transaction" {
		t.Fatal("search rewrote request-time admin snapshot", observed)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE users SET admin=true WHERE id=$1`, user)
	dbtest.GrantMember(t, d.Pool, org, user, &env, "audit_events.read")
	result, err := auditquery.Search(t.Context(), d.Pool, policy, p, f)
	if err != nil || len(result.Events) != 1 || result.NextBefore != nil {
		t.Fatalf("environment read: %+v %v", result, err)
	}
	if result.Events[0].ID != eventIDs[0] {
		t.Fatal("scope leak", result.Events[0])
	}
	f.EnvironmentID = other
	if _, err = auditquery.Search(t.Context(), d.Pool, policy, p, f); !errors.Is(err, auditquery.ErrSearchDenied) {
		t.Fatalf("explicit unauthorized environment: %v", err)
	}
	for i := 0; i < 60; i++ {
		extra := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) SELECT $1,org_id,project_id,$2,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at FROM environments WHERE id=$3`, extra, fmt.Sprintf("extra-%d", i), env)
		dbtest.GrantMember(t, d.Pool, org, user, &extra, "audit_events.read")
	}
	f.EnvironmentID = env
	if result, err = auditquery.Search(t.Context(), d.Pool, policy, p, f); err != nil || len(result.Events) != 1 {
		t.Fatalf("narrow many grants: %+v %v", result, err)
	}
	f.EnvironmentID = uuid.Nil()
	if _, err = auditquery.Search(t.Context(), d.Pool, policy, p, f); !errors.Is(err, auditquery.ErrSearchInput) {
		t.Fatalf("oversized union should request narrowing: %v", err)
	}
	f.EnvironmentID = uuid.Nil()
	dbtest.GrantMember(t, d.Pool, org, user, nil, "audit_events.read")
	result, err = auditquery.Search(t.Context(), d.Pool, policy, p, f)
	if err != nil || len(result.Events) != 1 || result.NextBefore == nil {
		t.Fatalf("organization pagination: %+v %v", result, err)
	}
	f.Before = result.NextBefore
	dbtest.MustExec(t, t.Context(), d.Pool, `DELETE FROM role_grants WHERE member_id=$1`, member)
	if _, err = auditquery.Search(t.Context(), d.Pool, policy, p, f); !errors.Is(err, auditquery.ErrSearchDenied) {
		t.Fatalf("stale cursor kept authority: %v", err)
	}
	f.Before = nil
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,'audit','audit',convert_to('audit','UTF8'))`, key, org, project, env)
	dbtest.SetAPIKeyRole(t, d.Pool, key, "audit_events.read")
	machine := auth.Principal{Kind: auth.PrincipalKindAPIKey, OrgID: org, ProjectID: project.String(), EnvironmentID: env.String(), APIKeyID: key, CredentialVersion: 1}
	if result, err = auditquery.Search(t.Context(), d.Pool, policy, machine, f); err != nil || len(result.Events) != 1 {
		t.Fatalf("key read: %+v %v", result, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE api_keys SET credential_version=2 WHERE id=$1`, key)
	if _, err = auditquery.Search(t.Context(), d.Pool, policy, machine, f); !errors.Is(err, auditquery.ErrSearchDenied) {
		t.Fatalf("rotated key retained read: %v", err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE audit_events SET recorded_at=statement_timestamp()-interval '2 hours',expires_at=statement_timestamp()-interval '1 hour' WHERE id=$1`, eventIDs[0])
	f.OrgID = org
	f.Limit = 100
	if result, err = auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, f); err != nil || len(result.Events) != 2 {
		t.Fatalf("expired admin result: %+v %v", result, err)
	}
	foreignOrg := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,slug,name) VALUES($1,'foreign-audit','Foreign')`, foreignOrg)
	foreignFilter := f
	foreignFilter.OrgID = foreignOrg
	foreignFilter.ActorKind = "user"
	foreignFilter.TargetKind = "slack_user_link"
	foreignFilter.TargetUserID = user
	foreignFilter.SlackTeamID = "T123"
	foreignFilter.SlackUserID = "U123"
	if _, err := auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, foreignFilter); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT bool_and(authorization_evidence->'checks' @> '[{"type":"user_attribute","attribute":"admin"}]'::jsonb AND details->>'actor_kind'='user' AND details->>'target_kind'='slack_user_link' AND details->>'target_user_id'=$2 AND details->>'slack_team_id'='T123' AND details->>'slack_user_id'='U123') FROM audit_events WHERE org_id=$1 AND action='audit.searched'`, foreignOrg, user.String()).Scan(&valid); err != nil || !valid {
		t.Fatalf("foreign membership or lost filters: %v %v", valid, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `CREATE FUNCTION reject_audit_search() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='audit.searched' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`)
	dbtest.MustExec(t, t.Context(), d.Pool, `CREATE TRIGGER reject_audit_search BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_audit_search()`)
	result, err = auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, f)
	if err == nil || len(result.Events) != 0 {
		t.Fatalf("released rows without access receipt: %+v %v", result, err)
	}
}

func TestSearchFiltersByActionsOperationAndEvent(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	org, user, member := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,slug,name) VALUES($1,'filters','Filters')`, org)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name,primary_email,admin) VALUES($1,'Auditor','auditor@example.test',true)`, user)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, member, org, user)
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	operation := uuid.NewV7()
	ids := map[string]uuid.UUID{}
	for _, e := range []struct {
		action    string
		operation uuid.UUID
	}{{"secret.created", operation}, {"secret.revoked", operation}, {"secret.rotated", uuid.Nil()}} {
		id := uuid.NewV7()
		ids[e.action] = id
		kind := "secret"
		err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, policy, audit.Event{ID: id, OperationID: e.operation, Evidence: audit.Internal(audit.ControlPlane), OrgID: org, Action: e.action, TargetKind: kind, TargetID: uuid.NewV7()})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	p := auth.Principal{Kind: auth.PrincipalKindSession, OrgID: org, UserID: user, MemberID: member, SessionID: uuid.NewV7(), Admin: true}
	search := func(f auditquery.SearchFilter) []uuid.UUID {
		t.Helper()
		f.OrgID, f.From, f.To, f.Limit = org, time.Now().Add(-time.Hour), time.Now().Add(time.Minute), 100
		result, err := auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, f)
		if err != nil {
			t.Fatal(err)
		}
		found := []uuid.UUID{}
		for _, event := range result.Events {
			if event.Action != "audit.searched" {
				found = append(found, event.ID)
			}
		}
		return found
	}
	if got := search(auditquery.SearchFilter{Actions: []string{"secret.created", "secret.rotated"}}); len(got) != 2 || got[0] != ids["secret.rotated"] || got[1] != ids["secret.created"] {
		t.Fatalf("any of several actions: %v", got)
	}
	if got := search(auditquery.SearchFilter{OperationID: operation}); len(got) != 2 {
		t.Fatalf("one operation's events: %v", got)
	}
	if got := search(auditquery.SearchFilter{EventID: ids["secret.revoked"]}); len(got) != 1 || got[0] != ids["secret.revoked"] {
		t.Fatalf("one event: %v", got)
	}
	bad := auditquery.SearchFilter{OrgID: org, From: time.Now().Add(-time.Hour), To: time.Now(), Limit: 10, Actions: []string{"secret.created", "not.an_action"}}
	if _, err := auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, bad); !errors.Is(err, auditquery.ErrSearchInput) {
		t.Fatal("unknown action accepted", err)
	}
	bad.Actions = make([]string, 101)
	for i := range bad.Actions {
		bad.Actions[i] = "secret.created"
	}
	if _, err := auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, bad); !errors.Is(err, auditquery.ErrSearchInput) {
		t.Fatal("unbounded action list accepted", err)
	}
	// The search records what it looked for.
	var recorded string
	if err := d.Pool.QueryRow(t.Context(), `SELECT details->>'event_id' FROM audit_events WHERE action='audit.searched' AND details ? 'event_id' ORDER BY recorded_at DESC LIMIT 1`).Scan(&recorded); err != nil || recorded != ids["secret.revoked"].String() {
		t.Fatalf("search detail %q %v", recorded, err)
	}
}
