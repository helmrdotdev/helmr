package slack

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAgentManagementUsesExactCurrentGrantAndStoredConnection(t *testing.T) {
	f, store, org := credentialFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM role_grants WHERE environment_id=$1`, f.Environment)
	// Ownership deliberately remains set while all operational authority is absent.
	if _, err := GetConnection(t.Context(), f.Pool, f.Principal(t), f.Environment, f.Agent, auth.PermissionAgentConnectionsRead); !errors.Is(err, errInstallationAuthority) {
		t.Fatalf("owner-only connection read: %v", err)
	}
	dbtest.GrantMember(t, f.Pool, org, f.User, &f.Environment, "agent_connections.read", "agent_connections.update_credentials", "agent_connections.authorize", "agent_connections.delete")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE role_grants SET all_agents=false WHERE environment_id=$1; INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$2 FROM role_grants WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Agent)
	otherAgent, otherRegistration, otherInstallation, otherConnection := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'other');
 INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id) SELECT transport,$3,org_id,'other-app','other-client',credential_revision,credential_ciphertext,credential_nonce,created_by_user_id FROM slack_app_registrations WHERE id=$7;
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id,authorization_lost_at) SELECT $4,$3,org_id,'other-app','other-team','other-bot',credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id,clock_timestamp()-interval '1 day' FROM slack_installations WHERE id=$8;
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) VALUES($5,$1,$2,$3,$4,$6,(SELECT org_id FROM slack_app_registrations WHERE id=$3));
 UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$8`, pgx.QueryExecModeSimpleProtocol, f.Environment, otherAgent, otherRegistration, otherInstallation, otherConnection, f.User, f.registration, f.installation)
	if view, err := GetConnection(t.Context(), f.Pool, f.Principal(t), f.Environment, f.Agent, auth.PermissionAgentConnectionsRead); err != nil || view == nil {
		t.Fatalf("exact connection: %+v %v", view, err)
	}
	if _, err := GetConnection(t.Context(), f.Pool, f.Principal(t), f.Environment, otherAgent, auth.PermissionAgentConnectionsRead); !errors.Is(err, errInstallationAuthority) {
		t.Fatalf("other Agent: %v", err)
	}
	if err := store.StoreAppCredentials(t.Context(), f.Principal(t), otherRegistration, AppCredentials{ClientID: "replacement", ClientSecret: "secret", SigningSecret: "signing"}); !errors.Is(err, errInstallationAuthority) {
		t.Fatalf("foreign registration: %v", err)
	}
	if err := store.DisconnectConnection(t.Context(), f.Principal(t), f.Environment, otherConnection); !errors.Is(err, errInstallationAuthority) {
		t.Fatalf("foreign connection: %v", err)
	}
	views, count, err := ReauthorizationRequired(t.Context(), f.Pool, f.Principal(t), f.Environment, 1)
	if err != nil || count != 1 || len(views) != 1 || views[0].ConnectionID != f.publication {
		t.Fatalf("filter must precede count and limit: %+v %d %v", views, count, err)
	}
	if _, err := ListSessionDelivery(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Principal(t), f.Environment, f.Session, 0, 100); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatalf("manager read run delivery: %v", err)
	}
	if err := RecoverDelivery(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, f.Principal(t), f.Environment, f.Session, uuid.NewV7(), DeliveryRecovery{AttemptID: uuid.NewV7(), PublicationRevision: 1}, "abandon"); !errors.Is(err, errInstallationAuthority) {
		t.Fatalf("manager changed run delivery: %v", err)
	}
	// The externally exchanged grant must be rejected if authorization changed in flight.
	observed, err := store.connectionAuthorization(t.Context(), f.Principal(t), f.publication)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM role_grants WHERE environment_id=$1`, f.Environment)
	if _, err = store.attachConnectionGrant(t.Context(), f.Principal(t), f.publication, observed, refreshResult().grant); !errors.Is(err, errInstallationAuthority) {
		t.Fatalf("revoked manager attached credentials: %v", err)
	}
	var revision int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT credential_revision FROM slack_installations WHERE id=$1`, f.installation).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("denied exchange changed credentials: %d %v", revision, err)
	}
}

func TestConnectionGrantCannotAdoptReplacementAuthority(t *testing.T) {
	for _, change := range []string{"logout", "credential", "proof", "membership", "suspended"} {
		t.Run(change, func(t *testing.T) {
			f, store, _ := credentialFixture(t)
			prior := f.Principal(t)
			observed, err := store.connectionAuthorization(t.Context(), prior, f.publication)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "logout":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, prior.SessionID)
			case "credential":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id=$1`, prior.IdentityID)
			case "proof":
				fresh := f.Principal(t)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET identity_id=$2,authenticated_at=clock_timestamp() WHERE id=$1`, prior.SessionID, fresh.IdentityID)
			case "membership":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET removed_at=clock_timestamp(),is_owner=false WHERE id=$1`, prior.MemberID)
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO org_members(id,org_id,user_id,is_owner) VALUES($1,$2,$3,true)`, uuid.NewV7(), prior.OrgID, f.User)
			case "suspended":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET suspended_at=clock_timestamp() WHERE id=$1`, prior.MemberID)
			}
			if _, err = store.attachConnectionGrant(t.Context(), prior, f.publication, observed, refreshResult().grant); !errors.Is(err, errInstallationAuthority) {
				t.Fatal("stale management source changed connection", err)
			}
			var revision int64
			if err = f.Pool.QueryRow(t.Context(), `SELECT credential_revision FROM slack_installations WHERE id=$1`, f.installation).Scan(&revision); err != nil || revision != 1 {
				t.Fatal(revision, err)
			}
		})
	}
}

func TestConnectionAttentionHidesRetiredSource(t *testing.T) {
	f, _, _ := credentialFixture(t)
	prior := f.Principal(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, prior.SessionID)
	if _, _, err := ReauthorizationRequired(t.Context(), f.Pool, prior, f.Environment, 10); !errors.Is(err, errInstallationAuthority) {
		t.Fatal(err)
	}
	if _, err := CountReauthorizationRequired(t.Context(), f.Pool, prior, f.Environment); !errors.Is(err, errInstallationAuthority) {
		t.Fatal(err)
	}
}
