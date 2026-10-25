package org_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMemberManagementSeparatesAccessAndOwnership(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	for _, m := range []org.ManagingMember{f.member, f.peer} {
		if _, _, err := inviteOne(ctx, f.pool, f.tokenKey, m, org.InvitationInput{Emails: []string{"blocked@example.test"}}); !errors.Is(err, org.ErrMemberManagementRequired) {
			t.Fatalf("invite without grant: %v", err)
		}
		if _, err := org.ListMembers(ctx, f.pool, m); !errors.Is(err, org.ErrMemberManagementRequired) {
			t.Fatalf("list without grant: %v", err)
		}
	}
	if _, err := org.UpdateMemberOwnership(ctx, f.pool, f.admin, f.member.MemberID, true, false); !errors.Is(err, org.ErrOwnerRoleRequired) {
		t.Fatalf("administrator appointed owner: %v", err)
	}
	if err := identity.RemoveMember(ctx, f.pool, f.admin, f.owner.MemberID); !errors.Is(err, org.ErrOwnerRoleRequired) {
		t.Fatalf("administrator removed owner: %v", err)
	}
	if _, err := org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.owner.MemberID, false, true); !errors.Is(err, org.ErrLastActiveOwner) {
		t.Fatalf("last owner removed: %v", err)
	}
	changed, err := org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.member.MemberID, true, false)
	if err != nil || !changed.Member.IsOwner {
		t.Fatalf("appoint owner: %v %v", changed, err)
	}
	if _, err = org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.member.MemberID, false, false); !errors.Is(err, org.ErrMemberRoleChanged) {
		t.Fatalf("stale ownership: %v", err)
	}
	if _, err = org.ListMembers(ctx, f.pool, f.member); !errors.Is(err, org.ErrMemberManagementRequired) {
		t.Fatalf("ownership granted access: %v", err)
	}
	recovered, err := org.RecoverOwnAccess(ctx, f.pool, f.member)
	if err != nil || !recovered.Created {
		t.Fatalf("recover: %v %v", recovered, err)
	}
	again, err := org.RecoverOwnAccess(ctx, f.pool, f.member)
	if err != nil || again.Created || again.ID != recovered.ID {
		t.Fatalf("recovery retry: %v %v", again, err)
	}
	if _, err = org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.member.MemberID, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err = org.ListMembers(ctx, f.pool, f.member); err != nil {
		t.Fatalf("demotion removed explicit grant: %v", err)
	}
	invitation, token, err := inviteOne(ctx, f.pool, f.tokenKey, f.admin, org.InvitationInput{Emails: []string{"Teammate@Example.test"}})
	if err != nil || token == "" || invitation.InviteeEmail != "teammate@example.test" {
		t.Fatalf("invite: %v %v", invitation, err)
	}
	if _, _, err = inviteOne(ctx, f.pool, f.tokenKey, f.admin, org.InvitationInput{Emails: []string{"teammate@example.test"}}); !errors.Is(err, org.ErrInvitationPending) {
		t.Fatalf("duplicate: %v", err)
	}
	if err = identity.RemoveMember(ctx, f.pool, f.admin, f.peer.MemberID); err != nil {
		t.Fatal(err)
	}
}

func TestCreateOrganizationPostgresInitialSetupAdmitsOneOrganization(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	userID := insertUser(t, database.Pool, "Owner", "owner@example.test")
	created, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "Acme", OwnerUserID: userID, InitialSetup: true})
	if err != nil || created.Slug != "acme" || created.Name != "acme" {
		t.Fatalf("organization = %+v, err = %v", created, err)
	}
	var admin bool
	if err := database.Pool.QueryRow(t.Context(), `SELECT admin FROM users WHERE id = $1`, userID).Scan(&admin); err != nil || !admin {
		t.Fatalf("initial owner admin = %t, err = %v", admin, err)
	}
	member, err := queries.GetActiveOrgMemberByUser(t.Context(), db.GetActiveOrgMemberByUserParams{OrgID: created.ID, UserID: pgvalue.UUID(userID)})
	if err != nil || member.RemovedAt.Valid || !member.IsOwner {
		t.Fatalf("owner member = %+v, err = %v", member, err)
	}
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "second", OwnerUserID: userID, InitialSetup: true}); !errors.Is(err, org.ErrOrganizationExists) {
		t.Fatalf("second setup error = %v", err)
	}
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "acme", OwnerUserID: userID}); !errors.Is(err, org.ErrOrganizationSlugInUse) {
		t.Fatalf("duplicate slug error = %v", err)
	}
}

type memberFixture struct {
	pool     *pgxpool.Pool
	queries  *db.Queries
	tokenKey []byte
	owner    org.ManagingMember
	admin    org.ManagingMember
	member   org.ManagingMember
	peer     org.ManagingMember
}

func TestMemberRemovalRevokesOnlyItsLoginSessionsAndRollsBackFailure(t *testing.T) {
	f := newMemberFixture(t)
	otherOrg := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Other','other')`, otherOrg)
	for _, scope := range []any{f.owner.OrgID, nil, otherOrg} {
		id := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO login_sessions(id,user_id,org_id,token_hash,expires_at) VALUES($1,$2,$3,$4,now()+interval '1 hour')`, id, f.member.UserID, scope, []byte(id.String()))
	}
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_member_session_revocation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected session revocation failure'; END $$;
 CREATE TRIGGER reject_member_session_revocation BEFORE UPDATE ON login_sessions FOR EACH STATEMENT EXECUTE FUNCTION reject_member_session_revocation()`)
	if err := identity.RemoveMember(t.Context(), f.pool, f.owner, f.member.MemberID); err == nil {
		t.Fatal("revocation failure committed member removal")
	}
	var intact bool
	if err := f.pool.QueryRow(t.Context(), `SELECT removed_at IS NULL AND (SELECT count(*) FROM login_sessions WHERE user_id=$2 AND revoked_at IS NULL)=3 FROM org_members WHERE org_id=$1 AND user_id=$2`, f.owner.OrgID, f.member.UserID).Scan(&intact); err != nil || !intact {
		t.Fatalf("member or sessions changed after failed removal: %v %v", intact, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_member_session_revocation ON login_sessions; DROP FUNCTION reject_member_session_revocation()`)
	if err := identity.RemoveMember(t.Context(), f.pool, f.owner, f.member.MemberID); err != nil {
		t.Fatal(err)
	}
	var correct bool
	if err := f.pool.QueryRow(t.Context(), `SELECT removed_at IS NOT NULL AND (SELECT bool_and((revoked_at IS NULL)=(org_id IS NOT DISTINCT FROM $3::uuid)) FROM login_sessions WHERE user_id=$2) FROM org_members WHERE org_id=$1 AND user_id=$2`, f.owner.OrgID, f.member.UserID, otherOrg).Scan(&correct); err != nil || !correct {
		t.Fatalf("member removal changed the revocation scope: %v %v", correct, err)
	}
}

func TestInitialOrganizationRollsBackFailedAdministratorGrant(t *testing.T) {
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	user := insertUser(t, database.Pool, "Owner", "owner@example.test")
	dbtest.MustExec(t, t.Context(), database.Pool, `CREATE FUNCTION reject_initial_admin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected administrator grant failure'; END $$;
 CREATE TRIGGER reject_initial_admin BEFORE UPDATE ON users FOR EACH STATEMENT EXECUTE FUNCTION reject_initial_admin()`)
	if _, err := identity.CreateOrganization(t.Context(), database.Pool, org.OrganizationInput{Actor: audit.System("test-fixture"), Slug: "initial", OwnerUserID: user, InitialSetup: true}); err == nil {
		t.Fatal("failed administrator grant created an organization")
	}
	var intact bool
	if err := database.Pool.QueryRow(t.Context(), `SELECT NOT admin AND NOT EXISTS(SELECT 1 FROM organizations) AND NOT EXISTS(SELECT 1 FROM org_members) FROM users WHERE id=$1`, user).Scan(&intact); err != nil || !intact {
		t.Fatalf("initial setup did not roll back: %v %v", intact, err)
	}
}

func newMemberFixture(t *testing.T) memberFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	orgID := uuid.NewV7()
	if _, err := queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Members", Slug: "members",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), database.Pool, func(tx pgx.Tx) error { return org.SeedStandardRoles(t.Context(), tx, orgID, audit.System("fixture")) }); err != nil {
		t.Fatal(err)
	}
	member := func(name, email, role string) org.ManagingMember {
		userID := insertUser(t, database.Pool, name, email)
		record, err := queries.EnsureOrgMember(t.Context(), db.EnsureOrgMemberParams{ID: pgvalue.NewUUIDv7(), OrgID: pgvalue.UUID(orgID), UserID: pgvalue.UUID(userID), IsOwner: role == "owner"})
		if err != nil {
			t.Fatal(err)
		}
		m := org.ManagingMember{Actor: audit.System("fixture"), OrgID: orgID, UserID: userID, MemberID: pgvalue.MustUUIDValue(record.ID)}
		if role == "owner" || role == "admin" {
			dbtest.MustExec(t, t.Context(), database.Pool, `INSERT INTO role_grants(id,org_id,member_id,role_id) SELECT $1,$2,$3,id FROM roles WHERE org_id=$2 AND builtin_key='access_administrator'`, uuid.NewV7(), orgID, m.MemberID)
		}
		if role == "owner" {
			dbtest.MustExec(t, t.Context(), database.Pool, `INSERT INTO role_grants(id,org_id,member_id,role_id) SELECT $1,$2,$3,id FROM roles WHERE org_id=$2 AND builtin_key='organization_structure_administrator'`, uuid.NewV7(), orgID, m.MemberID)
		}
		return m
	}
	keys, err := auth.NewKeys(bytes.Repeat([]byte{7}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return memberFixture{
		pool:     database.Pool,
		queries:  queries,
		tokenKey: keys.Invitation,
		owner:    member("Owner", "owner@example.test", "owner"),
		admin:    member("Admin", "admin@example.test", "admin"),
		member:   member("Member", "member@example.test", "member"),
		peer:     member("Peer", "peer@example.test", "member"),
	}
}

func insertUser(t *testing.T, pool *pgxpool.Pool, name string, email string) uuid.UUID {
	t.Helper()
	userID := uuid.NewV7()
	if _, err := pool.Exec(t.Context(), `INSERT INTO users (id, display_name, primary_email) VALUES ($1, $2, $3)`, userID, name, email); err != nil {
		t.Fatal(err)
	}
	return userID
}

func TestAcceptInvitationPostgres(t *testing.T) {
	fixture := newMemberFixture(t)
	ctx := t.Context()
	q := fixture.queries
	invite := func(email string, role string) org.PendingInvitation {
		t.Helper()
		_, rawToken, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, fixture.owner, org.InvitationInput{Emails: []string{email}})
		if err != nil {
			t.Fatal(err)
		}
		tokenHash, err := auth.HashToken(fixture.tokenKey, rawToken)
		if err != nil {
			t.Fatal(err)
		}
		invitation, err := org.PendingInvitationByTokenHash(ctx, q, tokenHash)
		if err != nil {
			t.Fatal(err)
		}
		byID, err := org.PendingInvitationByID(ctx, q, invitation.ID)
		if err != nil || byID != invitation {
			t.Fatalf("invitation by id = %+v, err = %v", byID, err)
		}
		return invitation
	}
	activeSessions := func(userID uuid.UUID) int {
		t.Helper()
		var count int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM login_sessions WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	session := func(userID uuid.UUID) {
		t.Helper()
		if _, err := fixture.pool.Exec(ctx, `INSERT INTO login_sessions (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`, uuid.NewV7(), userID, uuid.NewV7().String()); err != nil {
			t.Fatal(err)
		}
	}

	// The membership write satisfies the deferred invitation acceptance
	// constraint, so acceptance runs on a transaction.
	accept := func(invitation org.PendingInvitation, userID uuid.UUID, displayName string) error {
		return db.RunTx(ctx, fixture.pool, func(tx pgx.Tx) error {
			return identity.AcceptInvitation(ctx, tx, audit.VerifiedLogin(userID, "test-provider"), invitation, userID, displayName)
		})
	}
	if _, err := org.PendingInvitationByTokenHash(ctx, q, []byte("unknown")); !errors.Is(err, org.ErrInvitationNotFound) {
		t.Fatalf("unknown invitation error = %v", err)
	}
	joiner := insertUser(t, fixture.pool, "Joiner", "joiner@example.test")
	session(joiner)
	invitation := invite("joiner@example.test", "member")
	if err := accept(invitation, joiner, "Joiner"); err != nil {
		t.Fatal(err)
	}
	member, err := q.GetActiveOrgMemberByUser(ctx, db.GetActiveOrgMemberByUserParams{OrgID: pgvalue.UUID(fixture.owner.OrgID), UserID: pgvalue.UUID(joiner)})
	if err != nil || member.RemovedAt.Valid || member.IsOwner || member.DisplayName.String != "Joiner" {
		t.Fatalf("member = %+v, err = %v", member, err)
	}
	if activeSessions(joiner) != 0 {
		t.Fatal("acceptance kept earlier login sessions")
	}
	if err := accept(invitation, joiner, "Joiner"); !errors.Is(err, org.ErrAlreadyMember) {
		t.Fatalf("active member error = %v", err)
	}

	// An accepted or revoked invitation cannot be accepted again, and a
	// rejected acceptance leaves sessions alone.
	outsider := insertUser(t, fixture.pool, "Outsider", "outsider@example.test")
	session(outsider)
	if err := accept(invitation, outsider, "Outsider"); !errors.Is(err, org.ErrInvitationNotFound) {
		t.Fatalf("accepted invitation error = %v", err)
	}
	if activeSessions(outsider) != 1 {
		t.Fatal("rejected acceptance revoked sessions")
	}

	// A disabled membership is re-enabled with the invited role.
	if _, err := fixture.pool.Exec(ctx, `UPDATE org_members SET removed_at = now() WHERE org_id = $1 AND user_id = $2`, fixture.owner.OrgID, joiner); err != nil {
		t.Fatal(err)
	}
	reinvitation := invite("second@example.test", "member")
	if err := accept(reinvitation, joiner, ""); err != nil {
		t.Fatal(err)
	}
	if member, err := q.GetActiveOrgMemberByUser(ctx, db.GetActiveOrgMemberByUserParams{OrgID: pgvalue.UUID(fixture.owner.OrgID), UserID: pgvalue.UUID(joiner)}); err != nil || member.RemovedAt.Valid || member.IsOwner {
		t.Fatalf("re-enabled member = %+v, err = %v", member, err)
	}
	var newGrant bool
	if err := fixture.pool.QueryRow(ctx, `SELECT NOT (details ? 'old_role') AND NOT (details ? 'new_role') AND actor_kind='verified_login' AND target_user_id=$2 FROM audit_events WHERE action='invitation.accepted' AND target_id=$1`, reinvitation.ID, joiner).Scan(&newGrant); err != nil || !newGrant {
		t.Fatal("reinvitation recorded disabled membership as active authority", newGrant, err)
	}
}

func TestOwnershipAuditNoOpAndRollback(t *testing.T) {
	f := newMemberFixture(t)
	ctx := t.Context()
	if _, err := org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.member.MemberID, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.member.MemberID, true, true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='member.ownership_changed' AND target_id=$1`, f.member.MemberID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count=%d %v", count, err)
	}
	dbtest.MustExec(t, ctx, f.pool, `CREATE FUNCTION fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit failure'; END $$; CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_audit()`)
	if _, err := org.UpdateMemberOwnership(ctx, f.pool, f.owner, f.member.MemberID, false, true); err == nil {
		t.Fatal("audit failure accepted ownership change")
	}
	var owner bool
	if err := f.pool.QueryRow(ctx, `SELECT is_owner FROM org_members WHERE id=$1`, f.member.MemberID).Scan(&owner); err != nil || !owner {
		t.Fatalf("rollback: %v %v", owner, err)
	}
}

// inviteOne invites a single address and returns its invitation and the raw
// token delivered for it, or the error that refused the request or the address.
func inviteOne(ctx context.Context, database db.TxBeginner, tokenKey []byte, inviter org.ManagingMember, input org.InvitationInput) (db.Invitation, string, error) {
	var delivered org.NewInvitation
	results, err := org.CreateInvitations(ctx, database, tokenKey, inviter, input, func(_ context.Context, invitations []org.NewInvitation) error {
		delivered = invitations[0]
		return nil
	})
	if err != nil {
		return db.Invitation{}, "", err
	}
	if results[0].Err != nil {
		return db.Invitation{}, "", results[0].Err
	}
	return delivered.Invitation, delivered.RawToken, nil
}

// recordDelivery delivers nothing and records each batch it was given.
func recordDelivery(batches *[][]org.NewInvitation) org.DeliverInvitations {
	return func(_ context.Context, invitations []org.NewInvitation) error {
		*batches = append(*batches, invitations)
		return nil
	}
}

func TestCreateInvitationsPostgresDeliversOneBatchAndRefusesAddressesOnTheirOwn(t *testing.T) {
	fixture := newMemberFixture(t)
	ctx := t.Context()
	admin := fixture.admin
	if _, _, err := inviteOne(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"old@example.test"}}); err != nil {
		t.Fatal(err)
	}

	var batches [][]org.NewInvitation
	results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{
		Emails: []string{" First@Example.test", "not-an-address", "member@example.test", "first@example.test", "old@example.test", "second@example.test", "not-an-address"},
	}, recordDelivery(&batches))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		email string
		err   error
	}{
		{"first@example.test", nil},
		{"not-an-address", nil},
		{"member@example.test", org.ErrInvitationActiveMember},
		{"old@example.test", org.ErrInvitationPending},
		{"second@example.test", nil},
	}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %d distinct addresses", results, len(want))
	}
	for i, expected := range want {
		result := results[i]
		if result.Email != expected.email {
			t.Fatalf("result %d email = %q, want %q", i, result.Email, expected.email)
		}
		switch {
		case expected.email == "not-an-address":
			var input org.InputError
			if !errors.As(result.Err, &input) {
				t.Fatalf("invalid address result = %+v", result)
			}
		case expected.err != nil:
			if !errors.Is(result.Err, expected.err) {
				t.Fatalf("%s result = %+v, want %v", expected.email, result, expected.err)
			}
		default:
			if result.Err != nil {
				t.Fatalf("%s result = %+v", expected.email, result)
			}
			var facts int
			if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events JOIN invitations ON invitations.id = audit_events.target_id WHERE action='invitation.created' AND invitee_email=$1 AND audit_events.org_id=$2 AND NOT (details ? 'new_role')`, expected.email, admin.OrgID).Scan(&facts); err != nil || facts != 1 {
				t.Fatalf("%s audit facts = %d, err = %v", expected.email, facts, err)
			}
		}
	}
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("delivered batches = %+v, want one batch of the two new invitations", batches)
	}
	for i, i2 := range []int{0, 4} {
		delivered := batches[0][i]
		if delivered.Invitation.InviteeEmail != results[i2].Email || delivered.RawToken == "" {
			t.Fatalf("delivered %d = %+v, want %s", i, delivered, results[i2].Email)
		}
		tokenHash, err := auth.HashToken(fixture.tokenKey, delivered.RawToken)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := org.PendingInvitationByTokenHash(ctx, fixture.queries, tokenHash)
		if err != nil || pending.InviteeEmail != results[i2].Email {
			t.Fatalf("delivered token resolves to %+v, err = %v", pending, err)
		}
	}

	t.Run("invitations are written and delivered in address order", func(t *testing.T) {
		var ordered [][]org.NewInvitation
		results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"zed@example.test", "amy@example.test", "mia@example.test"}}, recordDelivery(&ordered))
		if err != nil || len(results) != 3 || results[0].Email != "zed@example.test" || results[1].Email != "amy@example.test" {
			t.Fatalf("results = %+v, err = %v, want request order", results, err)
		}
		if len(ordered) != 1 || len(ordered[0]) != 3 {
			t.Fatalf("batches = %+v", ordered)
		}
		for i, want := range []string{"amy@example.test", "mia@example.test", "zed@example.test"} {
			if got := ordered[0][i].Invitation.InviteeEmail; got != want {
				t.Fatalf("delivered %d = %s, want %s", i, got, want)
			}
		}
	})

	t.Run("nothing to deliver skips delivery", func(t *testing.T) {
		var none [][]org.NewInvitation
		results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{Emails: []string{"first@example.test", "bad"}}, recordDelivery(&none))
		if err != nil || len(results) != 2 || results[0].Err == nil || results[1].Err == nil || len(none) != 0 {
			t.Fatalf("results = %+v, batches = %d, err = %v", results, len(none), err)
		}
	})

	t.Run("a failed delivery rolls every invitation of the request back", func(t *testing.T) {
		sendErr := errors.New("provider unavailable")
		results, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, org.InvitationInput{
			Emails: []string{"third@example.test", "member@example.test", "fourth@example.test"},
		}, func(context.Context, []org.NewInvitation) error { return sendErr })
		if err != nil {
			t.Fatal(err)
		}
		for i, address := range []string{"third@example.test", "fourth@example.test"} {
			result := results[i*2]
			if result.Email != address || !errors.Is(result.Err, org.ErrInvitationNotDelivered) || !errors.Is(result.Err, sendErr) {
				t.Fatalf("%s result = %+v", address, result)
			}
		}
		if !errors.Is(results[1].Err, org.ErrInvitationActiveMember) {
			t.Fatalf("refused address result = %+v", results[1])
		}
		var invitations, facts int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitee_email IN ('third@example.test', 'fourth@example.test')`).Scan(&invitations); err != nil || invitations != 0 {
			t.Fatalf("invitations after failed delivery = %d, err = %v", invitations, err)
		}
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='invitation.created' AND target_id NOT IN (SELECT id FROM invitations)`).Scan(&facts); err != nil || facts != 0 {
			t.Fatalf("audit facts after failed delivery = %d, err = %v", facts, err)
		}
	})

	t.Run("the request is rejected as a whole", func(t *testing.T) {
		tooMany := make([]string, org.MaxInvitationsPerRequest+1)
		for i := range tooMany {
			tooMany[i] = fmt.Sprintf("bulk-%d@example.test", i)
		}
		var rejected [][]org.NewInvitation
		var input org.InputError
		for name, request := range map[string]org.InvitationInput{
			"no addresses": {},
			"over limit":   {Emails: tooMany},
		} {
			if _, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, admin, request, recordDelivery(&rejected)); !errors.As(err, &input) {
				t.Fatalf("%s error = %v, want input error", name, err)
			}
		}

		if _, err := org.CreateInvitations(ctx, fixture.pool, fixture.tokenKey, fixture.member, org.InvitationInput{Emails: []string{"dev-bulk@example.test"}}, recordDelivery(&rejected)); !errors.Is(err, org.ErrMemberManagementRequired) {
			t.Fatalf("member batch error = %v", err)
		}
		var leaked int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitee_email LIKE 'bulk-%' OR invitee_email IN ('role@example.test', 'owner-bulk@example.test', 'dev-bulk@example.test')`).Scan(&leaked); err != nil || leaked != 0 || len(rejected) != 0 {
			t.Fatalf("rejected requests created %d invitations, delivered %d batches, err = %v", leaked, len(rejected), err)
		}
	})
}
