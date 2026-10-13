package secret

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestSecretAuditCommitsOnceAndRollsBackMutationOnFailure(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, d.Pool)
	store, err := New(db.New(d.Pool), d.Pool, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7()
	actor := audit.Actor{Kind: "api_key", CredentialID: key}
	created, err := store.Create(t.Context(), actor, env, "TOKEN", []byte("private-value"), "create")
	if err != nil {
		t.Fatal(err)
	}
	id := pgvalue.MustUUIDValue(created.ID)
	if _, err = store.Create(t.Context(), actor, env, "TOKEN", []byte("private-value"), "create"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), actor, env, id, []byte("rotated-value"), "rotate"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rotate(t.Context(), actor, env, id, []byte("rotated-value"), "rotate"); err != nil {
		t.Fatal(err)
	}
	var count int
	var principal, scope, target bool
	if err = d.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(actor_kind='api_key' AND actor_credential_id=$1 AND actor_user_id IS NULL),bool_and(org_id IS NOT NULL AND project_id IS NOT NULL AND environment_id=$2),bool_and(target_kind='secret' AND target_id=$3 AND outcome='succeeded') FROM audit_events`, key, env, id).Scan(&count, &principal, &scope, &target); err != nil || count != 2 || !principal || !scope || !target {
		t.Fatalf("audit count=%d actor=%v scope=%v target=%v err=%v", count, principal, scope, target, err)
	}
	before := currentSecretVersion(t, d.Pool, env, created.ID)
	dbtest.MustExec(t, t.Context(), d.Pool, `CREATE FUNCTION reject_secret_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	dbtest.MustExec(t, t.Context(), d.Pool, `CREATE TRIGGER reject_secret_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_secret_audit()`)
	if _, err = store.Rotate(t.Context(), actor, env, id, []byte("failed-rotation"), "failed"); err == nil {
		t.Fatal("audit failure allowed rotation")
	}
	if got := currentSecretVersion(t, d.Pool, env, created.ID); got != before {
		t.Fatal("audit failure committed new secret version")
	}
	if _, err = store.Revoke(t.Context(), actor, env, id, "revoke"); err == nil {
		t.Fatal("audit failure allowed revocation")
	}
	var status string
	if err = d.Pool.QueryRow(t.Context(), `SELECT status FROM secrets WHERE id=$1`, id).Scan(&status); err != nil || status != "active" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `DROP TRIGGER reject_secret_audit ON audit_events`)
	if _, err = store.Revoke(t.Context(), actor, env, id, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Revoke(t.Context(), actor, env, id, ""); err != nil {
		t.Fatal(err)
	}
	if err = d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("revoke audit count=%d err=%v", count, err)
	}
}
