package slack

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackUserLinksNeverMergeAndPreserveHistoricalAdmissions(t *testing.T) {
	f, _, org := credentialFixture(t)
	first := UserIdentity{TeamID: "team", SlackUserID: "human"}
	if err := LinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, f.installation, first); err != nil {
		t.Fatal(err)
	}
	accepted := f.reply(t, "already-admitted", false)
	if err := admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), accepted); err != nil {
		t.Fatal(err)
	}
	links, err := ListUserLinks(t.Context(), f.Pool, org, f.User, "", 50)
	if err != nil || len(links) != 1 {
		t.Fatal(links, err)
	}
	if err = LinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, f.installation, first); err != nil {
		t.Fatal(err)
	}
	retried, err := ListUserLinks(t.Context(), f.Pool, org, f.User, "", 50)
	if err != nil || !retried[0].LinkedAt.Equal(links[0].LinkedAt) {
		t.Fatal("retry renewed authority", err)
	}
	if err = LinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, f.installation, UserIdentity{TeamID: "team", SlackUserID: "other"}); !errors.Is(err, ErrUserLinkConflict) {
		t.Fatal("identity replaced without unlink", err)
	}
	delayed := f.reply(t, "delayed-before-unlink", false)
	if err = UnlinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, "team", "human"); err != nil {
		t.Fatal(err)
	}
	if err = LinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, f.installation, first); err != nil {
		t.Fatal(err)
	}
	if err = admitMessage(t.Context(), f.Pool, nil, f.admissionClient(t), delayed); err != nil {
		t.Fatal(err)
	}
	var exact bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT a.status='accepted' AND a.user_id=$1 AND b.status='rejected' AND b.error='identity_unlinked' FROM slack_requests a,slack_requests b WHERE a.id=$2 AND b.id=$3`, f.User, accepted, delayed).Scan(&exact); err != nil || !exact {
		t.Fatal("relink replayed gesture or erased accepted identity", err)
	}
	if err = UnlinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, "team", "other"); err != nil {
		t.Fatal(err)
	}
	links, err = ListUserLinks(t.Context(), f.Pool, org, f.User, "", 50)
	if err != nil || len(links) != 1 {
		t.Fatal("stale unlink removed replacement", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE org_id=$1 AND user_id=$2`, org, f.User)
	if err = LinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, f.User, f.installation, first); !errors.Is(err, ErrUserLinkDenied) {
		t.Fatal("disabled member linked", err)
	}
}
func TestSlackUserLinksCompetingUsersCannotStealIdentity(t *testing.T) {
	f, _, org := credentialFixture(t)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Other')`, other)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES(uuidv7(),$1,$2)`, org, other)
	dbtest.GrantMember(t, f.Pool, org, other, &f.Environment, "sessions.manage")
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, user := range []uuid.UUID{f.User, other} {
		go func() {
			<-ready
			results <- LinkUser(t.Context(), f.Pool, audit.System("test-fixture"), org, user, f.installation, UserIdentity{TeamID: "team", SlackUserID: "human"})
		}()
	}
	close(ready)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatal(a, b)
	}
	if a != nil && !errors.Is(a, ErrUserLinkConflict) || b != nil && !errors.Is(b, ErrUserLinkConflict) {
		t.Fatal(a, b)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_user_links WHERE team_id='team' AND slack_user_id='human'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestLinkWorkspaceAuthorizationPrecedesPagination(t *testing.T) {
	f, _, org := credentialFixture(t)
	ctx := t.Context()
	hidden, registration, connection, agent := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	// The later installation sorts before the authorized one, so filtering after
	// LIMIT 1 would incorrectly produce an empty first page.
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'hidden')`, f.Environment, agent)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT transport,$2,org_id,'hidden-app',client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id FROM slack_app_registrations WHERE id=$1`, f.registration, registration)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$3,org_id,'hidden-app','hidden-team',bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id FROM slack_installations WHERE id=$1`, f.installation, hidden, registration)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id) VALUES($1,$2,$3,$4,$5,$6)`, connection, f.Environment, agent, registration, hidden, f.User)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE role_grants SET all_agents=false WHERE org_id=$1 AND environment_id=$2`, org, f.Environment)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO role_grant_agents(grant_id,environment_id,agent_id) SELECT id,environment_id,$3 FROM role_grants WHERE org_id=$1 AND environment_id=$2`, org, f.Environment, f.Agent)
	for _, capability := range []string{"agents.use", "agents.manage", "sessions.manage"} {
		t.Run(capability, func(t *testing.T) {
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE roles SET permissions=ARRAY[$2::text] WHERE org_id=$1`, org, capability)
			page, err := ListLinkWorkspaces(ctx, f.Pool, org, f.User, uuid.Nil(), 1)
			if err != nil || len(page) != 1 || page[0].InstallationID != f.installation {
				t.Fatalf("authorized first page: %+v %v", page, err)
			}
			next, err := ListLinkWorkspaces(ctx, f.Pool, org, f.User, page[0].InstallationID, 1)
			if err != nil || len(next) != 0 {
				t.Fatalf("next page: %+v %v", next, err)
			}
			if _, err := CheckLinkWorkspace(ctx, f.Pool, org, f.User, hidden); !errors.Is(err, ErrUserLinkDenied) {
				t.Fatalf("hidden workspace: %v", err)
			}
			if _, err := CheckLinkWorkspace(ctx, f.Pool, org, f.User, f.installation); err != nil {
				t.Fatal(err)
			}
		})
	}
	dbtest.MustExec(t, ctx, f.Pool, `DELETE FROM role_grant_agents WHERE environment_id=$1`, f.Environment)
	if page, err := ListLinkWorkspaces(ctx, f.Pool, org, f.User, uuid.Nil(), 1); err != nil || len(page) != 0 {
		t.Fatalf("empty selection: %+v %v", page, err)
	}
}
