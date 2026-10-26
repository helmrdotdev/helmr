package workergroup

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestWorkerGroupAuditCommitsOnceAndRollsBack(t *testing.T) {
	f := newSupplyFixture(t)
	user := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO users(id,display_name) VALUES($1,'Operator')`, user)
	actor := audit.FromPrincipal(auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, SessionID: uuid.NewV7()})
	paused, err := PauseGroup(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, f.groupID(), f.group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PauseGroup(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, f.groupID(), f.group.ClaimVersion); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='worker_group.paused' AND actor_user_id IS NOT NULL AND actor_user_id=$1 AND credential_id=$2 AND target_id=$3 AND org_id IS NULL AND environment_id IS NULL`, actor.Actor.UserID, actor.Authentication.CredentialID, f.groupID()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pause audit count=%d err=%v", count, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_worker_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE TRIGGER reject_worker_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_worker_audit()`)
	if _, err := ActivateGroup(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, f.groupID(), paused.ClaimVersion); err == nil {
		t.Fatal("audit failure allowed activation")
	}
	after := f.currentGroup(t)
	if after.Status != "paused" || after.ClaimVersion != paused.ClaimVersion {
		t.Fatal("activation did not roll back")
	}
	if _, _, err := CreatePool(t.Context(), f.pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, f.groupID(), "failed", paused.ClaimVersion); err == nil {
		t.Fatal("audit failure allowed pool creation")
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_pools WHERE worker_group_id=$1`, f.groupID()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pool leaked: %d %v", count, err)
	}
}
