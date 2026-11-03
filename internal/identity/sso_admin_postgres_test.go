package identity

import (
	"errors"
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
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM browser_sso_proofs WHERE member_id=$1 AND revoked_at IS NULL`, member).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	return proof
}

func TestRequiringSSORevokesOnceAndAdmitsOnlySSOProof(t *testing.T) {
	f := newSSOAdminFixture(t)
	yes := true
	if _, err := UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &yes}); !errors.Is(err, sso.ErrNoRecoveryCodes) {
		t.Fatalf("required without recovery codes: %v", err)
	}
	codes, err := GenerateRecoveryCodes(t.Context(), f.pool, f.cfg, f.manager(t))
	if err != nil || len(codes) != 10 {
		t.Fatalf("codes = %d, %v", len(codes), err)
	}

	member := f.user(t, "Member", "member@example.test")
	memberID := f.member(t, f.org, member, "member")
	ordinaryCLI := f.cliSession(t, member, memberID, nil)
	ownerProof := f.liveProof(t, f.ownerMember)
	ssoCLI := f.cliSession(t, f.owner, f.ownerMember, &ownerProof)
	ordinaryBrowser := f.session(t, member, uuid.Nil())
	_, token := f.invitation(t, f.org, "new@example.test", "member")

	result, err := UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &yes})
	if err != nil || !result.Policy.Required || result.RevokedCLISessions != 1 {
		t.Fatalf("require = %+v, %v", result, err)
	}
	if !f.revoked(t, ordinaryCLI) || f.revoked(t, ssoCLI) {
		t.Fatal("requiring SSO must revoke CLI sessions approved without SSO and keep those approved with it")
	}
	if _, err := f.admits(t, ordinaryBrowser, f.org); !errors.Is(err, ErrSSORequired) {
		t.Fatalf("ordinary browser under required SSO: %v", err)
	}
	if p, err := f.admits(t, f.ownerRaw, f.org); err != nil || !p.SSORequired {
		t.Fatalf("SSO browser under required SSO = %+v, %v", p, err)
	}
	f.exec(t, `UPDATE org_members SET reauthenticate_after=clock_timestamp() WHERE id=$1`, f.ownerMember)
	if _, err := f.admits(t, f.ownerRaw, f.org); !errors.Is(err, ErrSSOReauthenticationRequired) {
		t.Fatalf("SSO proof before the member cutoff: %v", err)
	}
	f.exec(t, `UPDATE org_members SET reauthenticate_after=NULL WHERE id=$1`, f.ownerMember)

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
	if _, err := f.admits(t, ordinaryBrowser, f.org); err != nil {
		t.Fatalf("ordinary browser after returning to optional: %v", err)
	}
	if f.revoked(t, ssoCLI) {
		t.Fatal("returning to optional revoked a session")
	}
	if err := UnrequireSSO(t.Context(), f.pool, f.cfg.auditPolicy, audit.Internal(audit.ControlPlaneCLI), "acme"); err != nil {
		t.Fatal(err)
	}
}

func TestRecentProofUnderRequiredSSO(t *testing.T) {
	now := time.Now()
	proof := uuid.NewV7()
	base := auth.Principal{Kind: auth.PrincipalKindSession, Channel: "browser", SSORequired: true}
	for _, tc := range []struct {
		name string
		edit func(*auth.Principal)
		ok   bool
	}{
		{"fresh SSO proof", func(p *auth.Principal) { p.SSOProofID, p.SSOAuthenticatedAt = proof, now }, true},
		{"stale SSO proof and a fresh email code", func(p *auth.Principal) {
			p.SSOProofID, p.SSOAuthenticatedAt, p.AuthenticatedAt, p.ProofProvider = proof, now.Add(-5*time.Hour), now, "email"
		}, true},
		{"stale SSO proof and a fresh passkey", func(p *auth.Principal) {
			p.SSOProofID, p.SSOAuthenticatedAt, p.AuthenticatedAt = proof, now.Add(-5*time.Hour), now
		}, true},
		{"stale SSO proof and a fresh GitHub sign-in", func(p *auth.Principal) {
			p.SSOProofID, p.SSOAuthenticatedAt, p.AuthenticatedAt, p.ProofProvider = proof, now.Add(-5*time.Hour), now, "github"
		}, false},
		{"fresh email code without an SSO proof", func(p *auth.Principal) { p.AuthenticatedAt, p.ProofProvider = now, "email" }, false},
	} {
		p := base
		tc.edit(&p)
		if err := auth.RequireRecentProof(p); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	optional := auth.Principal{Kind: auth.PrincipalKindSession, Channel: "browser", SSOProofID: proof, SSOAuthenticatedAt: now}
	if err := auth.RequireRecentProof(optional); err != nil {
		t.Errorf("fresh SSO proof under optional SSO: %v", err)
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
	if _, err := codesAndRequire(t, f); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSSOConnection(t.Context(), f.pool, store, f.manager(t), f.connection); err != nil {
		t.Fatal(err)
	}
	var required bool
	var liveBindings int
	if err := f.pool.QueryRow(t.Context(), `SELECT sso_required,(SELECT count(*) FROM org_sso_member_bindings WHERE org_id=$1 AND retired_at IS NULL) FROM organizations WHERE id=$1`, f.org).Scan(&required, &liveBindings); err != nil {
		t.Fatal(err)
	}
	if required || liveBindings != 0 {
		t.Fatalf("after disabling SSO: required=%v live bindings=%d", required, liveBindings)
	}
}

func codesAndRequire(t *testing.T, f ssoAdminFixture) (SSOPolicyResult, error) {
	t.Helper()
	if _, err := GenerateRecoveryCodes(t.Context(), f.pool, f.cfg, f.manager(t)); err != nil {
		return SSOPolicyResult{}, err
	}
	yes := true
	return UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &yes})
}

func TestRecoveryCodeAdmitsAnOwnerWithoutTheIdentityProvider(t *testing.T) {
	f := newSSOAdminFixture(t)
	codes, err := GenerateRecoveryCodes(t.Context(), f.pool, f.cfg, f.manager(t))
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := UpdateSSOPolicy(t.Context(), f.pool, f.manager(t), SSOPolicyChange{Required: &yes}); err != nil {
		t.Fatal(err)
	}
	// The IdP is down: the owner signs in with an ordinary method only.
	raw := f.session(t, f.owner, uuid.Nil())
	account, _ := f.admits(t, raw, uuid.Nil())
	if _, err := f.admits(t, raw, f.org); !errors.Is(err, ErrSSORequired) {
		t.Fatalf("before recovery: %v", err)
	}
	if err := RecoverSSO(t.Context(), f.pool, f.cfg, account, f.org, "AAAAA-AAAAA-AAAAA-AAAAA-AAAAAA"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown code: %v", err)
	}
	if err := RecoverSSO(t.Context(), f.pool, f.cfg, account, f.org, " "+codes[0]+" "); err != nil {
		t.Fatal(err)
	}
	if p, err := f.admits(t, raw, f.org); err != nil || p.SSOProofID == uuid.Nil() {
		t.Fatalf("after recovery = %+v, %v", p, err)
	}
	if err := RecoverSSO(t.Context(), f.pool, f.cfg, account, f.org, codes[0]); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reused code: %v", err)
	}
	member := f.user(t, "Member", "member@example.test")
	f.member(t, f.org, member, "member")
	memberRaw := f.session(t, member, uuid.Nil())
	memberAccount, _ := f.admits(t, memberRaw, uuid.Nil())
	if err := RecoverSSO(t.Context(), f.pool, f.cfg, memberAccount, f.org, codes[1]); !errors.Is(err, org.ErrOwnerRoleRequired) {
		t.Fatalf("a member used a recovery code: %v", err)
	}
	var notified int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM account_security_notifications WHERE event='sso.recovery_code_used'`).Scan(&notified); err != nil || notified == 0 {
		t.Fatalf("owners notified = %d, %v", notified, err)
	}
	remaining, _, err := sso.RecoveryCodes(t.Context(), f.pool, f.org)
	if err != nil || remaining != 9 {
		t.Fatalf("remaining = %d, %v", remaining, err)
	}
	if _, err := GenerateRecoveryCodes(t.Context(), f.pool, f.cfg, f.manager(t)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSSO(t.Context(), f.pool, f.cfg, account, f.org, codes[2]); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a code of a replaced set: %v", err)
	}
}

func TestCLISessionApprovedUnderRequiredSSORecordsTheProof(t *testing.T) {
	f := newSSOAdminFixture(t)
	if _, err := codesAndRequire(t, f); err != nil {
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

func TestSSOTestMarksOnlyUnchangedCandidate(t *testing.T) {
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
	started, err := StartSSOTest(t.Context(), f.pool, store, f.manager(t), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := started.Fingerprint()
	secret := "second"
	if err := UpdateSSOConnection(t.Context(), f.pool, store, f.manager(t), candidate.ID, SSOConnectionUpdate{ClientSecret: &secret}); err != nil {
		t.Fatal(err)
	}
	owner, _ := f.admits(t, f.ownerRaw, uuid.Nil())
	if err := CompleteSSOTest(t.Context(), f.pool, f.cfg, store, owner, f.org, f.ownerMember, candidate.ID, fingerprint); err == nil {
		t.Fatal("a test of earlier material marked the connection tested")
	}
	restarted, _ := StartSSOTest(t.Context(), f.pool, store, f.manager(t), candidate.ID)
	if err := CompleteSSOTest(t.Context(), f.pool, f.cfg, store, owner, f.org, f.ownerMember, candidate.ID, restarted.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if err := ActivateSSOConnection(t.Context(), f.pool, store, f.manager(t), candidate.ID); err != nil {
		t.Fatal(err)
	}
	// The previous connection retired with its bindings, so the owner's proof
	// through it no longer admits.
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT state FROM org_sso_connections WHERE id=$1`, f.connection).Scan(&state); err != nil || state != "retired" {
		t.Fatalf("previous connection state = %q, %v", state, err)
	}
	if p, err := f.admits(t, f.ownerRaw, f.org); err != nil || p.SSOProofID != uuid.Nil() {
		t.Fatalf("owner after replacement = %+v, %v", p, err)
	}
}
