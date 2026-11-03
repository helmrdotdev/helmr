package identity

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/sso"
)

type ssoFixture struct {
	identityFixture
	org        uuid.UUID
	connection uuid.UUID
}

// newSSOFixture creates an organization with an active SAML connection. The
// connection's material is never used: these tests start from a validated
// assertion.
func newSSOFixture(t *testing.T) ssoFixture {
	t.Helper()
	f := ssoFixture{identityFixture: newIdentityFixture(t)}
	f.org = f.organization(t, "acme")
	owner := f.member(t, f.org, f.user(t, "Owner", "owner@example.test"), "owner")
	f.connection = f.activeConnection(t, f.org, owner)
	return f
}

func (f ssoFixture) activeConnection(t *testing.T, orgID, creator uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	f.exec(t, `INSERT INTO sso_connections(id,org_id,protocol,state,saml_idp_entity_id,saml_sso_url,saml_certificates,created_by_member_id,activated_at)
 VALUES($1,$2,'saml','active',$4,'https://idp.example.test/sso',ARRAY['cert'],$3,clock_timestamp())`, id, orgID, creator, "https://idp.example.test/"+id.String())
	return id
}

func (f ssoFixture) assertion(subject, email string, at time.Time) SSOAssertion {
	return SSOAssertion{ConnectionID: f.connection, Protocol: sso.ProtocolSAML, Identity: sso.Identity{Subject: subject, Email: email, AuthenticatedAt: at}}
}

func (f ssoFixture) bind(t *testing.T, subject string, member, user uuid.UUID) {
	t.Helper()
	identity := uuid.NewV7()
	f.exec(t, `INSERT INTO sso_identities(id,org_id,connection_id,subject,user_id) VALUES($1,$2,$3,$4,$5)`, identity, f.org, f.connection, subject, user)
	f.exec(t, `INSERT INTO sso_member_bindings(id,org_id,identity_id,member_id,user_id) VALUES($1,$2,$3,$4,$5)`, uuid.NewV7(), f.org, identity, member, user)
}

func (f ssoFixture) admits(t *testing.T, raw string, target uuid.UUID) (auth.Principal, error) {
	t.Helper()
	return AuthenticateLoginSession(t.Context(), f.pool, f.cfg, raw, target)
}

func TestSSOSignInIssuesSessionThatEntersOnlyItsOrganization(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@example.test")
	member := f.member(t, f.org, user, "member")
	other := f.organization(t, "other")
	f.member(t, other, user, "member")
	f.bind(t, "alice-subject", member, user)

	result, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("alice-subject", "alice@example.test", time.Now().Add(-time.Minute)), "", uuid.Nil())
	if err != nil || result.Unbound || result.Session == "" || result.OrgID != f.org {
		t.Fatalf("sign-in = %+v, %v", result, err)
	}
	p, err := f.admits(t, result.Session, f.org)
	if err != nil || p.MemberID != member || p.SSOProofID == uuid.Nil() || !p.AuthenticatedAt.IsZero() {
		t.Fatalf("organization admission = %+v, %v", p, err)
	}
	if err := auth.RequireRecentProof(p); !errors.Is(err, auth.ErrRecentProofRequired) {
		t.Fatalf("SSO proof counted as ordinary recent proof: %v", err)
	}
	if _, err := f.admits(t, result.Session, other); !errors.Is(err, ErrOrganizationAccessDenied) {
		t.Fatalf("another organization admitted an SSO-only session: %v", err)
	}
	account, err := f.admits(t, result.Session, uuid.Nil())
	if err != nil || account.IdentityID != uuid.Nil() || account.PasskeyID != uuid.Nil() || account.SSOProofID != uuid.Nil() {
		t.Fatalf("account principal = %+v, %v", account, err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO login_sessions(id,user_id,channel,token_hash,org_id,member_id,expires_at) VALUES($1,$2,'cli','\x01',$3,$4,clock_timestamp()+interval '1 day')`, uuid.NewV7(), user, f.org, member); err == nil {
		t.Fatal("a CLI session without a source was accepted")
	}
}

func TestSSOSignInAddsProofToTheStartingSessionOfTheSameAccount(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@example.test")
	member := f.member(t, f.org, user, "member")
	f.bind(t, "alice-subject", member, user)
	raw := f.session(t, user, uuid.Nil())
	started, err := f.admits(t, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	a := f.assertion("alice-subject", "alice@example.test", time.Now())

	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, a, raw, uuid.Nil()); !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("a flow started signed out completed in a signed-in browser: %v", err)
	}
	result, err := SignInSSO(t.Context(), f.pool, f.cfg, a, raw, started.SessionID)
	if err != nil || result.Session != "" {
		t.Fatalf("sign-in = %+v, %v", result, err)
	}
	p, err := f.admits(t, raw, f.org)
	if err != nil || p.SSOProofID == uuid.Nil() || p.IdentityID == uuid.Nil() {
		t.Fatalf("admission = %+v, %v", p, err)
	}

	bob := f.user(t, "Bob", "bob@example.test")
	bobRaw := f.session(t, bob, uuid.Nil())
	bobSession, _ := f.admits(t, bobRaw, uuid.Nil())
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, a, bobRaw, bobSession.SessionID); !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("another account's browser received the proof: %v", err)
	}
}

func TestSSOSignInRefusesStaleAuthentication(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@example.test")
	member := f.member(t, f.org, user, "member")
	f.bind(t, "alice-subject", member, user)
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("alice-subject", "", time.Now().Add(-25*time.Hour)), "", uuid.Nil()); !errors.Is(err, ErrSSOReauthenticationRequired) {
		t.Fatalf("authentication older than the proof lifetime: %v", err)
	}
	f.exec(t, `UPDATE organizations SET sso_session_seconds=172800 WHERE id=$1`, f.org)
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("alice-subject", "", time.Now().Add(-25*time.Hour)), "", uuid.Nil()); err != nil {
		t.Fatalf("a longer organization lifetime still refused: %v", err)
	}
	f.exec(t, `UPDATE org_members SET reauthenticate_after=clock_timestamp() WHERE id=$1`, member)
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("alice-subject", "", time.Now().Add(-time.Minute)), "", uuid.Nil()); !errors.Is(err, ErrSSOReauthenticationRequired) {
		t.Fatalf("authentication before the member cutoff: %v", err)
	}
}

func TestSSOSignInWithoutBindingContinuesToAssociation(t *testing.T) {
	f := newSSOFixture(t)
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("new-subject", "new@example.test", time.Now().Add(-25*time.Hour)), "", uuid.Nil()); !errors.Is(err, ErrSSOReauthenticationRequired) {
		t.Fatalf("a stale authentication reached association: %v", err)
	}
	result, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("new-subject", "new@example.test", time.Now()), "", uuid.Nil())
	if err != nil || !result.Unbound || result.Session != "" {
		t.Fatalf("sign-in = %+v, %v", result, err)
	}
	f.exec(t, `UPDATE sso_connections SET state='retired',retired_at=clock_timestamp() WHERE id=$1`, f.connection)
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("new-subject", "", time.Now()), "", uuid.Nil()); !errors.Is(err, sso.ErrConnectionNotFound) {
		t.Fatalf("a retired connection signed in: %v", err)
	}
}

func TestSSOSelfLinkBindsTheMemberAndRefusesConflicts(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@login.test")
	member := f.member(t, f.org, user, "member")
	raw := f.session(t, user, uuid.Nil())
	p, err := f.admits(t, raw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	// The asserted email need not match the login email.
	a := f.assertion("alice-subject", "alice@corp.test", time.Now())
	if err := LinkSSO(t.Context(), f.pool, f.cfg, p, member, a); err != nil {
		t.Fatal(err)
	}
	if err := LinkSSO(t.Context(), f.pool, f.cfg, p, member, a); err != nil {
		t.Fatalf("repeating the same link: %v", err)
	}
	admitted, err := f.admits(t, raw, f.org)
	if err != nil || admitted.SSOProofID == uuid.Nil() {
		t.Fatalf("admission after self-link = %+v, %v", admitted, err)
	}
	if err := LinkSSO(t.Context(), f.pool, f.cfg, p, member, f.assertion("second-subject", "", time.Now())); !errors.Is(err, ErrSSOIdentityConflict) {
		t.Fatalf("a second identity for the member: %v", err)
	}
	bob := f.user(t, "Bob", "bob@example.test")
	bobMember := f.member(t, f.org, bob, "member")
	bobRaw := f.session(t, bob, uuid.Nil())
	bobP, _ := f.admits(t, bobRaw, uuid.Nil())
	if err := LinkSSO(t.Context(), f.pool, f.cfg, bobP, bobMember, a); !errors.Is(err, ErrSSOIdentityConflict) {
		t.Fatalf("another user's identity: %v", err)
	}
	if err := LinkSSO(t.Context(), f.pool, f.cfg, bobP, member, f.assertion("bob-subject", "", time.Now())); !errors.Is(err, ErrSSONotEligible) {
		t.Fatalf("linking another user's membership: %v", err)
	}
	f.exec(t, `UPDATE login_sessions SET authenticated_at=clock_timestamp()-interval '3 hours' WHERE id=$1`, bobP.SessionID)
	if err := LinkSSO(t.Context(), f.pool, f.cfg, bobP, bobMember, f.assertion("bob-subject", "", time.Now())); !errors.Is(err, ErrRecentProofRequired) {
		t.Fatalf("self-link without recent proof: %v", err)
	}
	var created int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='sso.binding_created' AND org_id=$1`, f.org).Scan(&created); err != nil || created != 1 {
		t.Fatalf("binding audit events = %d, %v", created, err)
	}
}

func TestSSOFirstAssociationByMembershipAndInvitation(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@example.test")
	f.member(t, f.org, user, "member")
	raw := f.session(t, user, uuid.Nil())
	p, _ := f.admits(t, raw, uuid.Nil())
	link := SSOLink{ConnectionID: f.connection, Protocol: sso.ProtocolSAML, Subject: "alice-subject", Email: "Alice@Example.test", AuthenticatedAt: time.Now()}

	mismatch := link
	mismatch.Email = "someone@example.test"
	if _, err := ConfirmSSOAssociation(t.Context(), f.pool, f.cfg, p, mismatch); !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("asserted email of another account: %v", err)
	}
	orgID, err := ConfirmSSOAssociation(t.Context(), f.pool, f.cfg, p, link)
	if err != nil || orgID != f.org {
		t.Fatalf("association = %v, %v", orgID, err)
	}
	if _, err := ConfirmSSOAssociation(t.Context(), f.pool, f.cfg, p, link); err != nil {
		t.Fatalf("a retry of the same association: %v", err)
	}
	if admitted, err := f.admits(t, raw, f.org); err != nil || admitted.SSOProofID == uuid.Nil() {
		t.Fatalf("admission after association = %+v, %v", admitted, err)
	}

	carol := f.user(t, "Carol", "carol@example.test")
	carolRaw := f.session(t, carol, uuid.Nil())
	carolP, _ := f.admits(t, carolRaw, uuid.Nil())
	carolLink := SSOLink{ConnectionID: f.connection, Protocol: sso.ProtocolSAML, Subject: "carol-subject", Email: "carol@example.test", AuthenticatedAt: time.Now()}
	if _, err := ConfirmSSOAssociation(t.Context(), f.pool, f.cfg, carolP, carolLink); !errors.Is(err, ErrSSONotEligible) {
		t.Fatalf("association without membership or invitation: %v", err)
	}
	_, token := f.invitation(t, f.org, "carol@example.test", "member")
	_, tokenHash, err := ResolveInvitation(t.Context(), f.queries, f.cfg, token)
	if err != nil {
		t.Fatal(err)
	}
	carolLink.InvitationTokenHash = tokenHash
	if _, err := ConfirmSSOAssociation(t.Context(), f.pool, f.cfg, carolP, carolLink); err != nil {
		t.Fatalf("association through an invitation: %v", err)
	}
	if admitted, err := f.admits(t, carolRaw, f.org); err != nil || admitted.SSOProofID == uuid.Nil() || admitted.MemberID == uuid.Nil() {
		t.Fatalf("admission after invited association = %+v, %v", admitted, err)
	}
}

func TestSSOProofEndsWithMembershipAndConnection(t *testing.T) {
	f := newSSOFixture(t)
	managerUser := f.user(t, "Manager", "manager@example.test")
	managerMember := f.member(t, f.org, managerUser, "owner")
	dbtest.GrantMember(t, f.pool, f.org, managerUser, nil, "members.remove")
	managerProof := dbtest.BrowserPrincipal(t, f.pool, f.org, managerUser)
	manager := org.ManagingMember{Human: managerProof, Actor: audit.FromPrincipal(managerProof), OrgID: f.org, UserID: managerUser, MemberID: managerMember, AuditPolicy: f.cfg.auditPolicy}
	signedIn := func(name string) (uuid.UUID, string) {
		user := f.user(t, name, name+"@example.test")
		member := f.member(t, f.org, user, "member")
		f.bind(t, name+"-subject", member, user)
		result, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion(name+"-subject", "", time.Now()), "", uuid.Nil())
		if err != nil {
			t.Fatal(err)
		}
		return member, result.Session
	}
	suspended, suspendedRaw := signedIn("suspended")
	yes := true
	if err := ChangeMemberAccess(t.Context(), f.pool, manager, suspended, &yes); err != nil {
		t.Fatal(err)
	}
	no := false
	if err := ChangeMemberAccess(t.Context(), f.pool, manager, suspended, &no); err != nil {
		t.Fatal(err)
	}
	var proofs int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sso_proofs WHERE member_id=$1 AND revoked_at IS NULL`, suspended).Scan(&proofs); err != nil || proofs != 0 {
		t.Fatalf("live proofs after suspension = %d, %v", proofs, err)
	}
	if _, err := f.admits(t, suspendedRaw, f.org); !errors.Is(err, ErrOrganizationAccessDenied) {
		t.Fatalf("a proof survived suspension: %v", err)
	}

	removed, removedRaw := signedIn("removed")
	if err := RemoveMember(t.Context(), f.pool, manager, removed); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sso_member_bindings WHERE member_id=$1 AND retired_at IS NULL`, removed).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live bindings after removal = %d, %v", live, err)
	}
	if _, err := f.admits(t, removedRaw, f.org); !errors.Is(err, ErrOrganizationAccessDenied) {
		t.Fatalf("a proof survived removal: %v", err)
	}

	_, keptRaw := signedIn("kept")
	if _, err := f.admits(t, keptRaw, f.org); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE sso_connections SET state='retired',retired_at=clock_timestamp() WHERE id=$1`, f.connection)
	if _, err := f.admits(t, keptRaw, f.org); !errors.Is(err, ErrOrganizationAccessDenied) {
		t.Fatalf("a proof survived connection retirement: %v", err)
	}
}

func TestOrdinarySignInKeepsSSOProofsOfTheSession(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@example.test")
	member := f.member(t, f.org, user, "member")
	f.bind(t, "alice-subject", member, user)
	result, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("alice-subject", "", time.Now()), "", uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	session, _ := f.admits(t, result.Session, uuid.Nil())
	_, err = SignInBrowserProvider(t.Context(), f.pool, f.cfg, ExternalIdentity{Provider: "github", Subject: "alice-github", Email: "alice@example.test", EmailVerified: true}, result.Session, session.SessionID)
	if !errors.Is(err, ErrAccountProofRequired) {
		t.Fatalf("an unlinked provider confirmed the account: %v", err)
	}
	f.exec(t, `INSERT INTO auth_identities(id,user_id,provider,subject) VALUES($1,$2,'github','alice-github')`, uuid.NewV7(), user)
	raw, err := SignInBrowserProvider(t.Context(), f.pool, f.cfg, ExternalIdentity{Provider: "github", Subject: "alice-github", Email: "alice@example.test", EmailVerified: true}, result.Session, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.admits(t, raw, f.org)
	if err != nil || p.SessionID != session.SessionID || p.IdentityID == uuid.Nil() || p.SSOProofID == uuid.Nil() {
		t.Fatalf("confirmed session = %+v, %v", p, err)
	}
}

// Member removal locks the member and then retires its binding. A concurrent
// sign-in takes the same order, so it waits for the removal and is refused
// instead of deadlocking with it.
func TestSSOSignInWaitsForConcurrentMemberRemoval(t *testing.T) {
	f := newSSOFixture(t)
	user := f.user(t, "Alice", "alice@example.test")
	member := f.member(t, f.org, user, "member")
	f.bind(t, "alice-subject", member, user)
	removal, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removal.Rollback(t.Context()) }()
	if _, err := removal.Exec(t.Context(), `SELECT id FROM org_members WHERE id=$1 FOR UPDATE`, member); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("alice-subject", "", time.Now()), "", uuid.Nil())
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := removal.Exec(t.Context(), `UPDATE sso_member_bindings SET retired_at=clock_timestamp() WHERE member_id=$1`, member); err != nil {
		t.Fatalf("removal blocked behind the sign-in: %v", err)
	}
	if _, err := removal.Exec(t.Context(), `UPDATE org_members SET removed_at=clock_timestamp() WHERE id=$1`, member); err != nil {
		t.Fatal(err)
	}
	if err := removal.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrSSONotEligible) {
		t.Fatalf("sign-in concurrent with removal: %v", err)
	}
}
