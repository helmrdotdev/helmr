package secret

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"time"
	"uuid"

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
	store, err := New(db.New(d.Pool), d.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) SELECT $1,org_id,project_id,id,'fixture','fixture',convert_to('fixture','UTF8') FROM environments WHERE id=$2`, key, env)
	dbtest.SetAPIKeyRole(t, d.Pool, key, "secrets.create", "secrets.rotate", "secrets.revoke")
	actor := auth.Principal{Kind: auth.PrincipalKindAPIKey, APIKeyID: key, CredentialVersion: 1}
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
	if err = d.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(actor_api_key_id IS NOT NULL AND credential_id=$1 AND actor_api_key_id=$1),bool_and(org_id IS NOT NULL AND project_id IS NOT NULL AND environment_id=$2),bool_and(target_kind='secret' AND target_id=$3 AND outcome='succeeded' AND details='{}'::jsonb) FROM audit_events`, key, env, id).Scan(&count, &principal, &scope, &target); err != nil || count != 2 || !principal || !scope || !target {
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

func TestSecretMutationPreservesTransactionCredentialFailure(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	env := seedSecretEnvironment(t, d.Pool)
	p := dbtest.SecretManager(t, d.Pool, env)
	store, err := New(db.New(d.Pool), d.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE api_keys SET credential_version=credential_version+1 WHERE id=$1`, p.APIKeyID)
	_, err = store.Create(t.Context(), p, env, "STALE", []byte("value"), "stale")
	var failure *access.PrincipalUnavailable
	if !errors.As(err, &failure) || failure.Reason != "credential_version_changed" || failure.PresentedVersion != 1 || failure.ObservedVersion != 2 {
		t.Fatalf("lost actual version comparison: %v", err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`, p.APIKeyID)
	_, err = store.Create(t.Context(), p, env, "REVOKED", []byte("value"), "revoked")
	if !errors.As(err, &failure) || failure.Reason != "current_key_not_found" {
		t.Fatalf("invented reason for unavailable key: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets WHERE environment_id=$1`, env).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestSecretMutationRechecksExactHumanSource(t *testing.T) {
	for _, retired := range []string{"logout", "source", "account", "membership", "old-proof"} {
		t.Run(retired, func(t *testing.T) {
			d := dbtest.Open(t)
			if err := schema.Up(t.Context(), d.DSN); err != nil {
				t.Fatal(err)
			}
			env := seedSecretEnvironment(t, d.Pool)
			var org uuid.UUID
			if err := d.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, env).Scan(&org); err != nil {
				t.Fatal(err)
			}
			user, member := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Manager')`, user)
			dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, member, org, user)
			dbtest.GrantMember(t, d.Pool, org, user, &env, "secrets.create", "secrets.rotate", "secrets.revoke")
			p := dbtest.BrowserPrincipal(t, d.Pool, org, user)
			store, err := New(db.New(d.Pool), d.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			row, err := store.Create(t.Context(), p, env, "ORIGINAL", []byte("original"), "initial")
			if err != nil {
				t.Fatal(err)
			}
			switch retired {
			case "logout":
				dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, p.SessionID)
			case "source":
				dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, p.IdentityID)
			case "account":
				dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE users SET access_epoch=access_epoch+1 WHERE id=$1`, user)
			case "membership":
				dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, member)
				dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, uuid.NewV7(), org, user)
				dbtest.GrantMember(t, d.Pool, org, user, &env, "secrets.create", "secrets.rotate", "secrets.revoke")
			case "old-proof":
				p.AuthenticatedAt = p.AuthenticatedAt.Add(-2 * time.Hour)
				dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE login_sessions SET authenticated_at=$2 WHERE id=$1`, p.SessionID, p.AuthenticatedAt)
			}
			_, createErr := store.Create(t.Context(), p, env, "DENIED", []byte("new"), "denied")
			_, rotateErr := store.Rotate(t.Context(), p, env, pgvalue.MustUUIDValue(row.ID), []byte("new"), "rotate")
			_, revokeErr := store.Revoke(t.Context(), p, env, pgvalue.MustUUIDValue(row.ID), "revoke")
			for _, err := range []error{createErr, rotateErr, revokeErr} {
				if !errors.Is(err, access.ErrDenied) && !errors.Is(err, auth.ErrRecentProofRequired) {
					t.Fatalf("retired source admitted mutation: %v", err)
				}
			}
		})
	}
}
