package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	auditquery "github.com/helmrdotdev/helmr/internal/audit/query"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
)

func TestAuditRetentionUsesCaptureTimeAndSurvivesRestore(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	user := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name,admin) VALUES($1,'Retention auditor',true)`, user)
	principal := auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, Admin: true, SessionID: uuid.NewV7()}
	old, newer := uuid.NewV7(), uuid.NewV7()
	appendFact := func(tx pgx.Tx, id uuid.UUID, seconds int64) error {
		return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: seconds}, audit.Event{ID: id, Evidence: audit.Internal(audit.ControlPlane), OccurredAt: time.Now().Add(-24 * time.Hour), Action: "secret.created", TargetKind: "secret", TargetID: uuid.NewV7()})
	}
	if err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error { return appendFact(tx, old, 3600) }); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error { return appendFact(tx, newer, 7200) }); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*)=2 AND bool_and(expires_at=recorded_at+retention_seconds*interval '1 second' AND occurred_at<recorded_at-interval '23 hours' AND retention_seconds=CASE WHEN id=$1 THEN 3600 ELSE 7200 END) FROM audit_events WHERE id=ANY($2::uuid[])`, old, []uuid.UUID{old, newer}).Scan(&correct); err != nil || !correct {
		t.Fatal("policy changed historical deadlines", correct, err)
	}
	// A transaction does not gain a fresh retention window when it finally commits.
	expired := uuid.NewV7()
	tx, err := d.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = appendFact(tx, expired, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM expires_at-clock_timestamp()))+0.02) FROM audit_events WHERE id=$1`, expired); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	filter := auditquery.SearchFilter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), Action: "secret.created", Limit: 100}
	result, err := auditquery.SearchInstallation(t.Context(), d.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, filter)
	if err != nil || len(result.Events) != 2 {
		t.Fatal("expired late commit disclosed", len(result.Events), err)
	}
	// A restore receipt cannot rewrite the restored facts or revive expired ones.
	if err = audit.RecordRestore(t.Context(), d.Pool, auditpolicy.Policy{RetentionSeconds: 86400}, audit.RestoreDetails{BackupReference: "fixture-backup", UnknownFrom: time.Now().Add(-time.Hour), UnknownTo: time.Now()}); err != nil {
		t.Fatal(err)
	}
	result, err = auditquery.SearchInstallation(t.Context(), d.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, principal, filter)
	if err != nil || len(result.Events) != 2 {
		t.Fatal("restore revived expiry", len(result.Events), err)
	}
	purged, err := audit.Purge(t.Context(), d.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, 1)
	if err != nil || purged.Deleted != 1 {
		t.Fatal("late commit cleanup", purged, err)
	}
}

func TestAuditDeletedOrganizationCustodyAndCompositeTargets(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	org, user, member, target := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,slug,name) VALUES($1,'original','Original')`, org)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name,admin) VALUES($1,'Auditor',true)`, user)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, member, org, user)
	dbtest.GrantMember(t, d.Pool, org, user, nil, "audit_events.read")
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	ids := []uuid.UUID{uuid.NewV7(), uuid.NewV7()}
	for i, team := range []string{"TONE", "TTWO"} {
		if err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, policy, audit.Event{ID: ids[i], Evidence: audit.Internal(audit.ControlPlane), OrgID: org, Action: "slack_user.linked", TargetKind: "slack_user_link", TargetUserID: target, SlackTeamID: team, SlackUserID: "UONE"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	principal := auth.Principal{Kind: auth.PrincipalKindSession, OrgID: org, UserID: user, MemberID: member, SessionID: uuid.NewV7(), Admin: true}
	filter := auditquery.SearchFilter{OrgID: org, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), TargetUserID: target, SlackTeamID: "TONE", SlackUserID: "UONE", Limit: 100}
	result, err := auditquery.Search(t.Context(), d.Pool, policy, principal, filter)
	if err != nil || len(result.Events) != 1 || result.Events[0].ID != ids[0] {
		t.Fatal("composite target filter", result, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `DELETE FROM organizations WHERE id=$1`, org)
	if _, err = auditquery.Search(t.Context(), d.Pool, policy, principal, filter); !errors.Is(err, auditquery.ErrSearchDenied) {
		t.Fatal("deleted membership kept custody", err)
	}
	result, err = auditquery.SearchInstallation(t.Context(), d.Pool, policy, principal, filter)
	if err != nil || len(result.Events) != 1 || result.Events[0].ID != ids[0] {
		t.Fatal("deleted history lost", result, err)
	}
	var retained bool
	if err = d.Pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(org_id IS NULL AND target_resolution='unresolved') FROM audit_events WHERE action='audit.searched' AND target_id=$1`, org).Scan(&retained); err != nil || !retained {
		t.Fatal("deleted access invented tenant", retained, err)
	}
	replacement, newMember := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,slug,name) VALUES($1,'original','Original')`, replacement)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, newMember, replacement, user)
	dbtest.GrantMember(t, d.Pool, replacement, user, nil, "audit_events.read")
	principal.OrgID, principal.MemberID = replacement, newMember
	filter.OrgID = replacement
	result, err = auditquery.Search(t.Context(), d.Pool, policy, principal, filter)
	if err != nil || len(result.Events) != 0 {
		t.Fatal("same-name organization inherited history", result, err)
	}
}
