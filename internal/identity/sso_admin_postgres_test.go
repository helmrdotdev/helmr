package identity

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/sso"
	"github.com/jackc/pgx/v5"
)

type ssoAdminFixture struct {
	ssoFixture
	owner       uuid.UUID
	ownerMember uuid.UUID
	ownerRaw    string
}

// newSSOAdminFixture adds an owner whose browser holds ordinary proof and an
// SSO proof through the active connection.
func newSSOAdminFixture(t *testing.T) ssoAdminFixture {
	t.Helper()
	f := ssoAdminFixture{ssoFixture: newSSOFixture(t)}
	f.owner = f.user(t, "Admin", "admin@example.test")
	f.ownerMember = f.member(t, f.org, f.owner, "owner")
	f.bind(t, "admin-subject", f.ownerMember, f.owner)
	f.ownerRaw = f.session(t, f.owner, uuid.Nil())
	session, err := f.admits(t, f.ownerRaw, uuid.Nil())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("admin-subject", "", time.Now()), f.ownerRaw, session.SessionID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f ssoAdminFixture) manager(t *testing.T) org.ManagingMember {
	t.Helper()
	p, err := f.admits(t, f.ownerRaw, f.org)
	if err != nil {
		t.Fatal(err)
	}
	return org.ManagingMember{Human: p, Actor: audit.FromPrincipal(p), OrgID: f.org, UserID: f.owner, MemberID: f.ownerMember, AuditPolicy: f.cfg.auditPolicy}
}

func (f ssoAdminFixture) cliSession(t *testing.T, user, member uuid.UUID, proof *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	if proof != nil {
		f.exec(t, `INSERT INTO login_sessions(id,user_id,channel,org_id,member_id,token_hash,sso_proof_id,authenticated_at,expires_at) VALUES($1,$2,'cli',$3,$4,uuid_send($1::uuid),$5,clock_timestamp(),clock_timestamp()+interval '1 day')`, id, user, f.org, member, *proof)
		return id
	}
	source := uuid.NewV7()
	f.exec(t, `INSERT INTO auth_identities(id,user_id,provider,subject) VALUES($1,$2,'fixture',$1::uuid::text)`, source, user)
	f.exec(t, `INSERT INTO login_sessions(id,user_id,channel,org_id,member_id,token_hash,identity_id,authenticated_at,expires_at) VALUES($1,$2,'cli',$3,$4,uuid_send($1::uuid),$5,clock_timestamp(),clock_timestamp()+interval '1 day')`, id, user, f.org, member, source)
	return id
}

func (f ssoAdminFixture) revoked(t *testing.T, session uuid.UUID) bool {
	t.Helper()
	var revoked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM login_sessions WHERE id=$1`, session).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	return revoked
}

func (f ssoAdminFixture) liveProof(t *testing.T, member uuid.UUID) uuid.UUID {
	t.Helper()
	var proof uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sso_proofs WHERE member_id=$1 AND revoked_at IS NULL`, member).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	return proof
}

func TestRequiringSSORevokesOnceAndAdmitsOnlySSOProof(t *testing.T) {
	f := newSSOAdminFixture(t)
	yes := true

	member := f.user(t, "Member", "member@example.test")
	memberID := f.member(t, f.org, member, "member")
	ordinaryCLI := f.cliSession(t, member, memberID, nil)
	ownerProof := f.liveProof(t, f.ownerMember)
	ssoCLI := f.cliSession(t, f.owner, f.ownerMember, &ownerProof)
	ownerOrdinaryCLI := f.cliSession(t, f.owner, f.ownerMember, nil)
	ordinaryBrowser := f.session(t, member, uuid.Nil())
	_, token := f.invitation(t, f.org, "new@example.test", "member")

	memberSession, _ := f.admits(t, ordinaryBrowser, uuid.Nil())
	device := uuid.NewV7()
	f.exec(t, `INSERT INTO device_codes(id,org_id,user_code_hash,device_code_hash,decided_by_member_id,user_id,browser_session_id,status,expires_at,poll_interval_seconds,decided_at)
 VALUES($1,$2,uuid_send($1::uuid),uuid_send(gen_random_uuid()),$3,$4,$5,'approved',clock_timestamp()+interval '5 minutes',5,clock_timestamp())`, device, f.org, memberID, member, memberSession.SessionID)
	// An owner who is not signed in through the connection cannot require it.
	otherOwner := f.user(t, "Other owner", "other@example.test")
	otherOwnerMember := f.member(t, f.org, otherOwner, "owner")
	otherP, err := f.admits(t, f.session(t, otherOwner, uuid.Nil()), f.org)
	if err != nil {
		t.Fatal(err)
	}
	other := org.ManagingMember{Human: otherP, Actor: audit.FromPrincipal(otherP), OrgID: f.org, UserID: otherOwner, MemberID: otherOwnerMember, AuditPolicy: f.cfg.auditPolicy}
	if _, err := UpdateSSOPolicy(t.Context(), f.pool, other, SSOPolicyChange{Required: &yes}); !errors.Is(err, sso.ErrEnforcerNotLinked) {
		t.Fatalf("requiring by an owner not signed in through the connection: %v", err)
	}
	slackLink := f.slackLink(t, member, memberID)

	result, err := UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &yes})
	if err != nil || !result.Policy.Required || result.RevokedCLISessions != 1 {
		t.Fatalf("require = %+v, %v", result, err)
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM device_codes WHERE id=$1`, device).Scan(&status); err != nil || status != "denied" {
		t.Fatalf("approved device code after requiring SSO = %q, %v", status, err)
	}
	if f.revoked(t, memberSession.SessionID) {
		t.Fatal("requiring SSO signed a browser out")
	}
	var slackRevoked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM slack_user_links WHERE id=$1`, slackLink).Scan(&slackRevoked); err != nil || !slackRevoked || result.RevokedSlackLinks != 1 {
		t.Fatalf("Slack link after requiring SSO: revoked=%v count=%d, %v", slackRevoked, result.RevokedSlackLinks, err)
	}
	if !f.revoked(t, ordinaryCLI) || f.revoked(t, ssoCLI) || f.revoked(t, ownerOrdinaryCLI) {
		t.Fatal("requiring SSO must revoke members' CLI sessions approved without SSO and keep those approved with it and owners'")
	}
	if _, err := f.admits(t, ordinaryBrowser, f.org); !errors.Is(err, ErrSSORequired) {
		t.Fatalf("ordinary browser under required SSO: %v", err)
	}
	if p, err := f.admits(t, f.ownerRaw, f.org); err != nil || !p.SSORequired {
		t.Fatalf("SSO browser under required SSO = %+v, %v", p, err)
	}
	// A member whose SSO proof predates their sign-in cutoff signs in again.
	f.bind(t, "member-subject", memberID, member)
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("member-subject", "", time.Now()), ordinaryBrowser, memberSession.SessionID); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE org_members SET reauthenticate_after=clock_timestamp() WHERE id=$1`, memberID)
	if _, err := f.admits(t, ordinaryBrowser, f.org); !errors.Is(err, ErrSSOReauthenticationRequired) {
		t.Fatalf("SSO proof before the member cutoff: %v", err)
	}

	newcomer := f.user(t, "New", "new@example.test")
	newcomerRaw := f.session(t, newcomer, uuid.Nil())
	newcomerP, _ := f.admits(t, newcomerRaw, uuid.Nil())
	if _, err := AcceptAccountInvitation(t.Context(), f.pool, f.cfg, newcomerP, token); !errors.Is(err, ErrSSORequired) {
		t.Fatalf("ordinary invitation acceptance under required SSO: %v", err)
	}

	no := false
	if _, err := UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &no}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE org_members SET reauthenticate_after=NULL WHERE id=$1`, memberID)
	if _, err := f.admits(t, ordinaryBrowser, f.org); err != nil {
		t.Fatalf("ordinary browser after returning to optional: %v", err)
	}
	if f.revoked(t, ssoCLI) {
		t.Fatal("returning to optional revoked a session")
	}
}

func TestRecentProofCountsOrdinaryOrSSOProof(t *testing.T) {
	now := time.Now()
	proof := uuid.NewV7()
	for _, tc := range []struct {
		name string
		p    auth.Principal
		ok   bool
	}{
		{"fresh SSO proof", auth.Principal{SSOProofID: proof, SSOAuthenticatedAt: now}, true},
		{"stale SSO proof and a fresh ordinary sign-in", auth.Principal{SSOProofID: proof, SSOAuthenticatedAt: now.Add(-5 * time.Hour), AuthenticatedAt: now}, true},
		{"stale SSO proof and a stale ordinary sign-in", auth.Principal{SSOProofID: proof, SSOAuthenticatedAt: now.Add(-5 * time.Hour), AuthenticatedAt: now.Add(-3 * time.Hour)}, false},
		{"fresh ordinary sign-in only", auth.Principal{AuthenticatedAt: now}, true},
	} {
		for _, required := range []bool{false, true} {
			p := tc.p
			p.Kind, p.Channel, p.SSORequired = auth.PrincipalKindSession, "browser", required
			if err := auth.RequireRecentProof(p); (err == nil) != tc.ok {
				t.Errorf("%s (required=%v): %v", tc.name, required, err)
			}
		}
	}
}

func TestUnlinkingAndRetiringRevokeWhatTheBindingIssued(t *testing.T) {
	f := newSSOAdminFixture(t)
	member := f.user(t, "Member", "member@example.test")
	memberID := f.member(t, f.org, member, "member")
	f.bind(t, "member-subject", memberID, member)
	memberRaw := f.session(t, member, uuid.Nil())
	memberSession, _ := f.admits(t, memberRaw, uuid.Nil())
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("member-subject", "", time.Now()), memberRaw, memberSession.SessionID); err != nil {
		t.Fatal(err)
	}
	proof := f.liveProof(t, memberID)
	ssoCLI := f.cliSession(t, member, memberID, &proof)
	ordinaryCLI := f.cliSession(t, member, memberID, nil)
	device := uuid.NewV7()
	f.exec(t, `INSERT INTO device_codes(id,org_id,user_code_hash,device_code_hash,decided_by_member_id,user_id,browser_session_id,status,expires_at,poll_interval_seconds,decided_at)
 VALUES($1,$2,uuid_send($1::uuid),uuid_send(gen_random_uuid()),$3,$4,$5,'approved',clock_timestamp()+interval '5 minutes',5,clock_timestamp())`, device, f.org, memberID, member, memberSession.SessionID)

	if err := UnlinkSSOMember(t.Context(), f.pool, f.manager(t), memberID); err != nil {
		t.Fatal(err)
	}
	if !f.revoked(t, ssoCLI) || f.revoked(t, ordinaryCLI) {
		t.Fatal("unlinking must revoke CLI sessions approved through the binding and only those")
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM device_codes WHERE id=$1`, device).Scan(&status); err != nil || status != "denied" {
		t.Fatalf("approved device code after unlink = %q, %v", status, err)
	}
	if p, err := f.admits(t, memberRaw, f.org); err != nil || p.SSOProofID != uuid.Nil() {
		t.Fatalf("unlinked member keeps ordinary access only = %+v, %v", p, err)
	}
	if err := UnlinkSSOMember(t.Context(), f.pool, f.manager(t), memberID); !errors.Is(err, org.ErrMemberNotFound) {
		t.Fatalf("unlinking twice: %v", err)
	}

	store, err := sso.NewStore(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requireSSO(t, f); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSSOConnection(t.Context(), f.pool, store, f.manager(t), f.connection); err != nil {
		t.Fatal(err)
	}
	var required bool
	var liveBindings int
	if err := f.pool.QueryRow(t.Context(), `SELECT sso_required,(SELECT count(*) FROM sso_member_bindings WHERE org_id=$1 AND retired_at IS NULL) FROM organizations WHERE id=$1`, f.org).Scan(&required, &liveBindings); err != nil {
		t.Fatal(err)
	}
	if required || liveBindings != 0 {
		t.Fatalf("after disabling SSO: required=%v live bindings=%d", required, liveBindings)
	}
}

func requireSSO(t *testing.T, f ssoAdminFixture) (SSOPolicyResult, error) {
	t.Helper()
	yes := true
	return UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &yes})
}

// Owners keep their ordinary sign-in under required SSO, so an identity
// provider outage cannot lock the organization out; members cannot.
func TestOwnersKeepOrdinarySignInUnderRequiredSSO(t *testing.T) {
	f := newSSOAdminFixture(t)
	if _, err := requireSSO(t, f); err != nil {
		t.Fatal(err)
	}
	p, err := f.admits(t, f.session(t, f.owner, uuid.Nil()), f.org)
	if err != nil || p.SSOProofID != uuid.Nil() || !p.SSORequired {
		t.Fatalf("owner with an ordinary sign-in = %+v, %v", p, err)
	}
	if err := auth.RequireRecentProof(p); err != nil {
		t.Fatalf("owner's fresh ordinary sign-in as recent proof: %v", err)
	}
	cli := f.cliSession(t, f.owner, f.ownerMember, nil)
	if _, err := access.SessionPrincipal(t.Context(), f.pool, cli, f.org); err != nil {
		t.Fatalf("owner's ordinary CLI session: %v", err)
	}
	member := f.user(t, "Member", "member@example.test")
	memberID := f.member(t, f.org, member, "member")
	if _, err := f.admits(t, f.session(t, member, uuid.Nil()), f.org); !errors.Is(err, ErrSSORequired) {
		t.Fatalf("member with an ordinary sign-in: %v", err)
	}
	memberCLI := f.cliSession(t, member, memberID, nil)
	if _, err := access.SessionPrincipal(t.Context(), f.pool, memberCLI, f.org); err == nil {
		t.Fatal("a member's ordinary CLI session was admitted under required SSO")
	}
}

func TestCLISessionApprovedUnderRequiredSSORecordsTheProof(t *testing.T) {
	f := newSSOAdminFixture(t)
	if _, err := requireSSO(t, f); err != nil {
		t.Fatal(err)
	}
	approver, err := f.admits(t, f.ownerRaw, f.org)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	err = db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		raw, err = issueCLISession(t.Context(), tx, f.cfg, approver, uuid.NewV7())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cli, err := f.admits(t, raw, uuid.Nil())
	if err != nil || cli.Channel != "cli" || cli.SSOProofID != approver.SSOProofID || cli.IdentityID != uuid.Nil() {
		t.Fatalf("CLI principal = %+v, %v", cli, err)
	}
}

func TestSSOTestActivationAndReplacement(t *testing.T) {
	f := newSSOAdminFixture(t)
	store, err := sso.NewStore(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m := f.manager(t)
	candidate, err := CreateSSOConnection(t.Context(), f.pool, store, m, SSOConnectionInput{OIDC: &sso.OIDCConfig{Issuer: "https://idp.example.test", ClientID: "helmr", ClientSecret: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateSSOConnection(t.Context(), f.pool, store, f.manager(t), candidate.ID); !errors.Is(err, sso.ErrUntested) {
		t.Fatalf("activating an untested candidate: %v", err)
	}
	if _, err := StartSSOTest(t.Context(), f.pool, store, f.manager(t), candidate.ID); err != nil {
		t.Fatal(err)
	}
	owner, _ := f.admits(t, f.ownerRaw, uuid.Nil())
	if err := CompleteSSOTest(t.Context(), f.pool, f.cfg, store, owner, f.org, f.ownerMember, candidate.ID); err != nil {
		t.Fatal(err)
	}
	secret := "second"
	if err := UpdateSSOConnection(t.Context(), f.pool, store, f.manager(t), candidate.ID, SSOConnectionUpdate{ClientSecret: &secret}); err != nil {
		t.Fatal(err)
	}
	if err := ActivateSSOConnection(t.Context(), f.pool, store, f.manager(t), candidate.ID); !errors.Is(err, sso.ErrUntested) {
		t.Fatalf("activating after an update cleared the test: %v", err)
	}
	if err := CompleteSSOTest(t.Context(), f.pool, f.cfg, store, owner, f.org, f.ownerMember, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := ActivateSSOConnection(t.Context(), f.pool, store, f.manager(t), candidate.ID); err != nil {
		t.Fatal(err)
	}
	// The previous connection retired with its bindings, so the owner's proof
	// through it no longer admits.
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT state FROM sso_connections WHERE id=$1`, f.connection).Scan(&state); err != nil || state != "retired" {
		t.Fatalf("previous connection state = %q, %v", state, err)
	}
	if p, err := f.admits(t, f.ownerRaw, f.org); err != nil || p.SSOProofID != uuid.Nil() {
		t.Fatalf("owner after replacement = %+v, %v", p, err)
	}
}

func (f ssoAdminFixture) approvedDevice(t *testing.T, user, member, session uuid.UUID) string {
	t.Helper()
	raw := "device-" + uuid.NewV7().String()
	hash, err := auth.HashToken(f.cfg.deviceCodeKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewV7()
	f.exec(t, `INSERT INTO device_codes(id,org_id,user_code_hash,device_code_hash,decided_by_member_id,user_id,browser_session_id,status,expires_at,poll_interval_seconds,decided_at)
 VALUES($1,$2,uuid_send($1::uuid),$3,$4,$5,$6,'approved',clock_timestamp()+interval '5 minutes',5,clock_timestamp())`, id, f.org, hash, member, user, session)
	return raw
}

// Requiring SSO holds the organization lock while it denies approved device
// codes; an exchange waits for it and then finds its code denied.
func TestCLIExchangeWaitsForRequiringSSO(t *testing.T) {
	f := newSSOAdminFixture(t)
	member := f.user(t, "Member", "member@example.test")
	memberID := f.member(t, f.org, member, "member")
	memberRaw := f.session(t, member, uuid.Nil())
	memberSession, _ := f.admits(t, memberRaw, uuid.Nil())
	device := f.approvedDevice(t, member, memberID, memberSession.SessionID)

	enforcement, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enforcement.Rollback(t.Context()) }()
	if _, err := org.LockSSOPolicy(t.Context(), enforcement, f.org); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ExchangeDeviceCode(t.Context(), f.pool, f.cfg, device)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if _, _, err := requireSSORevocations(t.Context(), enforcement, f.org); err != nil {
		t.Fatal(err)
	}
	if err := org.SetSSOPolicy(t.Context(), enforcement, f.org, org.SSOPolicy{Required: true, SessionSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	if err := enforcement.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("a CLI session was issued past the switch to required SSO")
	}
	var cli int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM login_sessions WHERE channel='cli' AND user_id=$1`, member).Scan(&cli); err != nil || cli != 0 {
		t.Fatalf("CLI sessions = %d, %v", cli, err)
	}
}

func TestCLISourcePrefersTheSSOProof(t *testing.T) {
	f := newSSOAdminFixture(t)
	// The owner's ordinary sign-in predates the cutoff; a later SSO sign-in
	// admits the browser again.
	f.exec(t, `UPDATE org_members SET reauthenticate_after=clock_timestamp() WHERE id=$1`, f.ownerMember)
	session, _ := f.admits(t, f.ownerRaw, uuid.Nil())
	if _, err := SignInSSO(t.Context(), f.pool, f.cfg, f.assertion("admin-subject", "", time.Now().Add(time.Second)), f.ownerRaw, session.SessionID); err != nil {
		t.Fatal(err)
	}
	approver, err := f.admits(t, f.ownerRaw, f.org)
	if err != nil || approver.SSOProofID == uuid.Nil() {
		t.Fatalf("approver = %+v, %v", approver, err)
	}
	var raw string
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		raw, err = issueCLISession(t.Context(), tx, f.cfg, approver, uuid.NewV7())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cli, err := f.admits(t, raw, uuid.Nil()); err != nil || cli.SSOProofID == uuid.Nil() {
		t.Fatalf("CLI principal = %+v, %v", cli, err)
	}
}

func (f ssoAdminFixture) slackLink(t *testing.T, user, member uuid.UUID) uuid.UUID {
	t.Helper()
	registration, installation, link := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.exec(t, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,created_by_user_id) VALUES('http',$1,$2,'app',$3)`, registration, f.org, user)
	f.exec(t, `INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id) VALUES($1,$2,$3,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),'{}',$4)`, installation, registration, f.org, user)
	f.exec(t, `INSERT INTO slack_user_links(id,installation_id,org_id,member_id,user_id,team_id,slack_user_id) VALUES($1,$2,$3,$4,$5,'team','slack-user')`, link, installation, f.org, member, user)
	return link
}

// A former owner loses what ownership kept under required SSO: their Slack
// links retire with the ownership, and their ordinary browser and CLI are
// refused at use.
func TestLosingOwnershipUnderRequiredSSO(t *testing.T) {
	f := newSSOAdminFixture(t)
	former := f.user(t, "Former owner", "former@example.test")
	formerMember := f.member(t, f.org, former, "owner")
	link := f.slackLink(t, former, formerMember)
	cli := f.cliSession(t, former, formerMember, nil)
	formerRaw := f.session(t, former, uuid.Nil())
	if _, err := requireSSO(t, f); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateMemberOwnership(t.Context(), f.pool, f.manager(t), formerMember, false, true); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM slack_user_links WHERE id=$1`, link).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("former owner's Slack link revoked=%v, %v", revoked, err)
	}
	if _, err := f.admits(t, formerRaw, f.org); !errors.Is(err, ErrSSORequired) {
		t.Fatalf("former owner's ordinary browser: %v", err)
	}
	if _, err := access.SessionPrincipal(t.Context(), f.pool, cli, f.org); err == nil {
		t.Fatal("former owner's ordinary CLI session was admitted")
	}
}
