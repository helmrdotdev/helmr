package slack

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func rejectSlackAudit(t *testing.T, f statusFixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_slack_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE TRIGGER reject_slack_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_slack_audit()`)
}

func TestSlackAuditCredentialAndDisconnectAtomicity(t *testing.T) {
	f, store, org := credentialFixture(t)
	actor := audit.FromPrincipal(auth.Principal{Kind: auth.PrincipalKindSession, UserID: f.User, SessionID: uuid.NewV7()})
	credentials := AppCredentials{ClientID: "client", ClientSecret: "replacement-secret", SigningSecret: "replacement-signing"}
	rejectSlackAudit(t, f)
	if err := store.StoreAppCredentials(t.Context(), actor, org, f.User, f.registration, credentials); err == nil {
		t.Fatal("audit failure allowed credential rotation")
	}
	if err := store.DisconnectConnection(t.Context(), actor, org, f.User, f.Environment, f.publication); err == nil {
		t.Fatal("audit failure allowed disconnect")
	}
	var intact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.credential_revision=1 AND r.retired_at IS NULL AND r.credential_ciphertext IS NOT NULL AND p.revoked_at IS NULL AND i.disconnected_at IS NULL AND i.credential_ciphertext IS NOT NULL FROM slack_app_registrations r JOIN slack_connections p ON p.slack_app_registration_id=r.id JOIN slack_installations i ON i.id=p.slack_installation_id WHERE p.id=$1`, f.publication).Scan(&intact); err != nil || !intact {
		t.Fatal("credential or disconnect mutation survived rollback", intact, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_slack_audit ON audit_events`)
	if err := store.StoreAppCredentials(t.Context(), actor, org, f.User, f.registration, credentials); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.DisconnectConnection(t.Context(), actor, org, f.User, f.Environment, f.publication); err != nil {
			t.Fatal(err)
		}
	}
	var configured, disconnected int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE action='slack_app.credentials_configured' AND target_id=$1),count(*) FILTER (WHERE action='slack_connection.disconnected' AND target_id=$2 AND environment_id=$3 AND project_id IS NOT NULL) FROM audit_events WHERE actor_user_id IS NOT NULL AND actor_user_id=$4 AND credential_id=$5 AND org_id=$6`, f.registration, f.publication, f.Environment, f.User, actor.Authentication.CredentialID, org).Scan(&configured, &disconnected); err != nil || configured != 1 || disconnected != 1 {
		t.Fatal("missing/duplicate scoped facts", configured, disconnected, err)
	}
}

func TestSlackAuditGrantFactsAndRollback(t *testing.T) {
	f, store, org := credentialFixture(t)
	actor := audit.FromPrincipal(auth.Principal{Kind: auth.PrincipalKindSession, UserID: f.User, SessionID: uuid.NewV7()})
	observed, err := store.connectionAuthorization(t.Context(), org, f.User, f.publication)
	if err != nil {
		t.Fatal(err)
	}
	grant := refreshResult().grant
	rejectSlackAudit(t, f)
	if _, err = store.attachConnectionGrant(t.Context(), actor, org, f.User, f.publication, observed, grant); err == nil {
		t.Fatal("audit failure allowed grant")
	}
	var intact bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT credential_revision=1 FROM slack_installations WHERE id=$1`, f.installation).Scan(&intact); err != nil || !intact {
		t.Fatal(intact, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_slack_audit ON audit_events`)
	if _, err = store.attachConnectionGrant(t.Context(), actor, org, f.User, f.publication, observed, grant); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='slack_connection.authorized' AND target_id=$1 AND actor_user_id=$2 AND credential_id=$3 AND org_id=$4 AND environment_id=$5 AND details->'permissions'=to_jsonb($6::text[]) AND NOT (details ? 'expires_at')`, f.publication, f.User, actor.Authentication.CredentialID, org, f.Environment, grant.scopes).Scan(&count); err != nil || count != 1 {
		t.Fatal("grant fact differs", count, err)
	}
}

func TestSlackAuditCompositeLinkFactsAndRollback(t *testing.T) {
	f, _, org := credentialFixture(t)
	actor := audit.FromPrincipal(auth.Principal{Kind: auth.PrincipalKindSession, UserID: f.User, SessionID: uuid.NewV7()})
	identity := UserIdentity{TeamID: "team", SlackUserID: "audit-human"}
	rejectSlackAudit(t, f)
	if err := LinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, org, f.User, f.installation, identity); err == nil {
		t.Fatal("audit failure allowed link")
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE slack_user_id='audit-human'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE audit_events DISABLE TRIGGER reject_slack_audit`)
	for range 2 {
		if err := LinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, org, f.User, f.installation, identity); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `ALTER TABLE audit_events ENABLE TRIGGER reject_slack_audit`)
	if err := UnlinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, org, f.User, identity.TeamID, identity.SlackUserID); err == nil {
		t.Fatal("audit failure allowed unlink")
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE slack_user_id='audit-human'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_slack_audit ON audit_events`)
	for range 2 {
		if err := UnlinkUser(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, actor, org, f.User, identity.TeamID, identity.SlackUserID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action IN ('slack_user.linked','slack_user.unlinked') AND target_kind='slack_user_link' AND target_id IS NULL AND target_user_id=$1 AND slack_team_id=$2 AND slack_user_id=$3 AND actor_user_id=$1 AND credential_id=$4 AND org_id=$5`, f.User, identity.TeamID, identity.SlackUserID, actor.Authentication.CredentialID, org).Scan(&count); err != nil || count != 2 {
		t.Fatal("composite facts missing or duplicated", count, err)
	}
}

func TestSlackAuditAuthorizationLossOnceAndRollback(t *testing.T) {
	f, _, org := credentialFixture(t)
	post := f.post(t, 1, "content", "lifecycle", nil)
	claim := f.postClaim(t, post)
	rejectSlackAudit(t, f)
	lose := func() error {
		return db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(t.Context(), `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, f.installation).Scan(&id); err != nil {
				return err
			}
			return recordAuthorizationLoss(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Internal(audit.ControlPlane), f.installation, claim.CredentialRevision, claim.AuthorizedAt)
		})
	}
	if err := lose(); err == nil {
		t.Fatal("audit failure allowed authorization loss")
	}
	var intact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT authorization_lost_at IS NULL FROM slack_installations WHERE id=$1`, f.installation).Scan(&intact); err != nil || !intact {
		t.Fatal(intact, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_slack_audit ON audit_events`)
	for range 2 {
		if err := lose(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='slack_installation.authorization_lost' AND target_id=$1 AND org_id=$2 AND actor_component IS NOT NULL AND actor_component='control_plane' AND actor_user_id IS NULL AND credential_id IS NULL`, f.installation, org).Scan(&count); err != nil || count != 1 {
		t.Fatal("loss fact missing or duplicated", count, err)
	}
}
