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
			return audit.Append(t.Context(), tx, policy, audit.Event{ID: id, Actor: audit.System("fixture"), OrgID: org, EnvironmentID: e, Action: "secret.created", TargetKind: "secret", TargetID: uuid.NewV7()})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	f := auditquery.SearchFilter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), Action: "secret.created", Limit: 1}
	p := auth.Principal{Kind: auth.PrincipalKindSession, OrgID: org, UserID: user, MemberID: member, SessionID: uuid.NewV7(), Admin: true}
	if _, err := auditquery.Search(t.Context(), d.Pool, policy, p, f); !errors.Is(err, auditquery.ErrSearchDenied) {
		t.Fatalf("admin flag granted tenant read: %v", err)
	}
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
	foreignFilter.ActorKind = "human"
	foreignFilter.TargetKind = "slack_user_link"
	foreignFilter.TargetUserID = user
	foreignFilter.SlackTeamID = "T123"
	foreignFilter.SlackUserID = "U123"
	if _, err := auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, foreignFilter); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT bool_and(actor_member_id IS NULL AND details->>'actor_kind'='human' AND details->>'target_kind'='slack_user_link' AND details->>'target_user_id'=$2 AND details->>'slack_team_id'='T123' AND details->>'slack_user_id'='U123') FROM audit_events WHERE org_id=$1 AND action='audit.searched'`, foreignOrg, user.String()).Scan(&valid); err != nil || !valid {
		t.Fatalf("foreign membership or lost filters: %v %v", valid, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `CREATE FUNCTION reject_audit_search() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='audit.searched' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`)
	dbtest.MustExec(t, t.Context(), d.Pool, `CREATE TRIGGER reject_audit_search BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_audit_search()`)
	result, err = auditquery.SearchInstallation(t.Context(), d.Pool, policy, p, f)
	if err == nil || len(result.Events) != 0 {
		t.Fatalf("released rows without access receipt: %+v %v", result, err)
	}
}
