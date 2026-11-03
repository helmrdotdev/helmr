package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/helmrdotdev/helmr/internal/sso"
	"github.com/jackc/pgx/v5"
)

// recoveryCodeCount codes of 128 random bits make one recovery set.
const recoveryCodeCount = 10

var ErrSSOSessionLifetime = errors.New("the single sign-on session lifetime must be between 1 and 30 days")

// ssoOwnerTx runs an SSO administration change under the organization row
// lock, for a live owner, with recent proof when recent is set.
func ssoOwnerTx(ctx context.Context, database db.TxBeginner, m org.ManagingMember, recent bool, fn func(tx pgx.Tx, current auth.Principal, policy org.SSOPolicy) error) error {
	if m.AuditPolicy.Validate() != nil {
		return errors.New("audit policy is required")
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		policy, err := org.LockSSOPolicy(ctx, tx, m.OrgID)
		if err != nil {
			return err
		}
		current, err := m.Current(ctx, tx)
		if err != nil {
			return err
		}
		if !current.IsOwner {
			return org.ErrOwnerRoleRequired
		}
		if recent {
			if err := auth.RequireRecentProof(current); err != nil {
				return err
			}
		}
		return fn(tx, current, policy)
	})
}

func ownerEvidence(current auth.Principal) audit.Evidence {
	e := audit.FromPrincipal(current)
	e.Authorization = audit.Ownership(current)
	return e
}

// SSOStatus is an organization's SSO configuration for its owners.
type SSOStatus struct {
	Policy            org.SSOPolicy
	Active            *sso.Connection
	Candidate         *sso.Connection
	MembersLinked     int
	MembersTotal      int
	RecoveryRemaining int
	RecoveryGenerated *time.Time
}

// ReadSSOStatus reads the configuration. Client secrets are never returned.
func ReadSSOStatus(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember) (SSOStatus, error) {
	var status SSOStatus
	err := ssoOwnerTx(ctx, database, m, false, func(tx pgx.Tx, _ auth.Principal, policy org.SSOPolicy) error {
		status.Policy = policy
		for _, read := range []struct {
			fn  func(context.Context, db.DBTX, uuid.UUID) (sso.Connection, error)
			dst **sso.Connection
		}{{store.Active, &status.Active}, {store.Candidate, &status.Candidate}} {
			c, err := read.fn(ctx, tx, m.OrgID)
			if errors.Is(err, sso.ErrConnectionNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			c.OIDC.ClientSecret = ""
			*read.dst = &c
		}
		members, err := sso.LinkedMembers(ctx, tx, m.OrgID)
		if err != nil {
			return err
		}
		status.MembersTotal = len(members)
		for _, member := range members {
			if member.LinkedAt != nil {
				status.MembersLinked++
			}
		}
		status.RecoveryRemaining, status.RecoveryGenerated, err = sso.RecoveryCodes(ctx, tx, m.OrgID)
		return err
	})
	return status, err
}

// SSOConnectionInput is a new candidate's trust configuration. An OIDC issuer
// has already passed discovery.
type SSOConnectionInput struct {
	OIDC *sso.OIDCConfig
	SAML *sso.SAMLIdP
}

// CreateSSOConnection records a candidate connection.
func CreateSSOConnection(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember, input SSOConnectionInput) (sso.Connection, error) {
	var created sso.Connection
	err := ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, _ org.SSOPolicy) error {
		var err error
		switch {
		case input.OIDC != nil:
			created, err = store.CreateOIDC(ctx, tx, m.OrgID, current.MemberID, *input.OIDC)
		case input.SAML != nil:
			created, err = store.CreateSAML(ctx, tx, m.OrgID, current.MemberID, *input.SAML)
		default:
			err = invalidInput("choose OIDC or SAML")
		}
		if err != nil {
			return err
		}
		created.OIDC.ClientSecret = ""
		return ssoConnectionEvent(ctx, tx, m, current, "sso.connection_created", created)
	})
	return created, err
}

// SSOConnectionUpdate replaces rotatable material: an OIDC client secret, or a
// SAML sign-in URL and signing certificates.
type SSOConnectionUpdate struct {
	ClientSecret *string
	SAML         *sso.SAMLIdP
}

// UpdateSSOConnection rotates material of a candidate or active connection. The
// connection must be tested again before a later activation.
func UpdateSSOConnection(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember, id uuid.UUID, update SSOConnectionUpdate) error {
	return ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, _ org.SSOPolicy) error {
		c, err := store.Get(ctx, tx, m.OrgID, id)
		if err != nil {
			return err
		}
		switch {
		case update.ClientSecret != nil && c.Protocol == sso.ProtocolOIDC:
			if *update.ClientSecret == "" {
				return invalidInput("the client secret is required")
			}
			err = store.RotateOIDCSecret(ctx, tx, m.OrgID, id, *update.ClientSecret)
		case update.SAML != nil && c.Protocol == sso.ProtocolSAML:
			idp := *update.SAML
			if idp.EntityID == "" {
				idp.EntityID = c.SAML.EntityID
			}
			if idp.EntityID != c.SAML.EntityID {
				return invalidInput("the identity provider entity ID cannot change; create a new connection")
			}
			if err := idp.Validate(); err != nil {
				return invalidInput("%s", err.Error())
			}
			err = store.UpdateSAMLMaterial(ctx, tx, m.OrgID, id, idp)
		default:
			err = invalidInput("only the client secret, or the SAML sign-in URL and certificates, can change; create a new connection to change its identity provider")
		}
		if err != nil {
			return err
		}
		return ssoConnectionEvent(ctx, tx, m, current, "sso.connection_updated", c)
	})
}

// StartSSOTest checks that the owner may test the candidate and returns it.
func StartSSOTest(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember, id uuid.UUID) (sso.Connection, error) {
	var c sso.Connection
	err := ssoOwnerTx(ctx, database, m, false, func(tx pgx.Tx, _ auth.Principal, _ org.SSOPolicy) error {
		var err error
		if c, err = store.Get(ctx, tx, m.OrgID, id); err != nil {
			return err
		}
		if c.State != "candidate" {
			return sso.ErrConnectionNotFound
		}
		return nil
	})
	return c, err
}

// CompleteSSOTest marks the candidate tested after the owner who started the
// test signed in through it, provided its material did not change meanwhile.
func CompleteSSOTest(ctx context.Context, database db.TxBeginner, cfg Config, store *sso.Store, p auth.Principal, orgID, member, id uuid.UUID, fingerprint []byte) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := org.LockSSOPolicy(ctx, tx, orgID); err != nil {
			return err
		}
		current, err := access.SessionPrincipal(ctx, tx, p.SessionID, orgID)
		if err != nil {
			return err
		}
		if current.UserID != p.UserID || current.MemberID != member || !current.IsOwner {
			return org.ErrOwnerRoleRequired
		}
		c, err := store.Get(ctx, tx, orgID, id)
		if err != nil {
			return err
		}
		if c.State != "candidate" || !bytes.Equal(c.Fingerprint(), fingerprint) {
			return invalidInput("the connection changed during the test; test it again")
		}
		if err := store.MarkTested(ctx, tx, orgID, id); err != nil {
			return err
		}
		m := org.ManagingMember{AuditPolicy: cfg.auditPolicy, OrgID: orgID}
		return ssoConnectionEvent(ctx, tx, m, current, "sso.connection_tested", c)
	})
}

// ActivateSSOConnection makes the tested candidate active, with optional SSO
// unless the policy already requires it. A previous active connection retires
// with its revocation set.
func ActivateSSOConnection(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember, id uuid.UUID) error {
	return ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, _ org.SSOPolicy) error {
		c, err := store.Get(ctx, tx, m.OrgID, id)
		if err != nil {
			return err
		}
		if c.State != "candidate" {
			return sso.ErrConnectionNotFound
		}
		if c.TestedAt == nil {
			return sso.ErrUntested
		}
		previous, bindings, err := sso.RetireActive(ctx, tx, m.OrgID)
		if err != nil {
			return err
		}
		if err := retireSSOBindings(ctx, tx, bindings); err != nil {
			return err
		}
		if previous.ID != uuid.Nil() {
			if err := ssoConnectionEvent(ctx, tx, m, current, "sso.connection_retired", previous); err != nil {
				return err
			}
		}
		if err := sso.Activate(ctx, tx, m.OrgID, id); err != nil {
			return err
		}
		return ssoConnectionEvent(ctx, tx, m, current, "sso.connection_activated", c)
	})
}

// DeleteSSOConnection discards a candidate, or retires the active connection,
// which turns SSO off for the organization and clears required SSO.
func DeleteSSOConnection(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember, id uuid.UUID) error {
	return ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, policy org.SSOPolicy) error {
		c, err := store.Get(ctx, tx, m.OrgID, id)
		if err != nil {
			return err
		}
		switch c.State {
		case "candidate":
			if err := store.DiscardCandidate(ctx, tx, m.OrgID, id); err != nil {
				return err
			}
		case "active":
			_, bindings, err := sso.RetireActive(ctx, tx, m.OrgID)
			if err != nil {
				return err
			}
			if err := retireSSOBindings(ctx, tx, bindings); err != nil {
				return err
			}
			if policy.Required {
				policy.Required = false
				if err := org.SetSSOPolicy(ctx, tx, m.OrgID, policy); err != nil {
					return err
				}
			}
		default:
			return sso.ErrConnectionNotFound
		}
		return ssoConnectionEvent(ctx, tx, m, current, "sso.connection_retired", c)
	})
}

// SSOPolicyChange sets the required switch, the session lifetime, or both. A
// nil SessionSeconds field leaves the lifetime unchanged; ResetSessionSeconds
// returns it to the default.
type SSOPolicyChange struct {
	Required            *bool
	SessionSeconds      *int32
	ResetSessionSeconds bool
}

// SSOPolicyResult reports what requiring SSO revoked.
type SSOPolicyResult struct {
	Policy             org.SSOPolicy
	RevokedCLISessions int
	RevokedSlackLinks  int
}

// UpdateSSOPolicy changes the policy. Requiring SSO needs an active connection
// and a live recovery-code set, and revokes once the organization's CLI
// sessions not approved through SSO, approved device codes not yet exchanged,
// Slack links and pending Slack link requests. Returning to optional revokes
// nothing.
func UpdateSSOPolicy(ctx context.Context, database db.TxBeginner, m org.ManagingMember, change SSOPolicyChange) (SSOPolicyResult, error) {
	var result SSOPolicyResult
	err := ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, before org.SSOPolicy) error {
		after := before
		if change.ResetSessionSeconds {
			after.SessionSeconds = nil
		} else if change.SessionSeconds != nil {
			if *change.SessionSeconds < 86400 || *change.SessionSeconds > 2592000 {
				return ErrSSOSessionLifetime
			}
			after.SessionSeconds = change.SessionSeconds
		}
		if change.Required != nil {
			after.Required = *change.Required
		}
		if after.Required && !before.Required {
			active, err := sso.HasActive(ctx, tx, m.OrgID)
			if err != nil {
				return err
			}
			if !active {
				return invalidInput("activate a connection before requiring single sign-on")
			}
			remaining, _, err := sso.RecoveryCodes(ctx, tx, m.OrgID)
			if err != nil {
				return err
			}
			if remaining == 0 {
				return sso.ErrNoRecoveryCodes
			}
			if result.RevokedCLISessions, result.RevokedSlackLinks, err = requireSSORevocations(ctx, tx, m.OrgID); err != nil {
				return err
			}
		}
		if err := org.SetSSOPolicy(ctx, tx, m.OrgID, after); err != nil {
			return err
		}
		result.Policy = after
		return ssoPolicyEvent(ctx, tx, m.AuditPolicy, ownerEvidence(current), m.OrgID, before, after, result)
	})
	return result, err
}

// UnrequireSSO returns an organization to optional SSO for a self-hosted
// operator, when its owners have lost both IdP access and every recovery code.
func UnrequireSSO(ctx context.Context, database db.TxBeginner, policy auditpolicy.Policy, actor audit.Evidence, slug string) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var orgID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE slug=$1`, slug).Scan(&orgID); err != nil {
			if isNoRows(err) {
				return invalidInput("organization %q not found", slug)
			}
			return err
		}
		before, err := org.LockSSOPolicy(ctx, tx, orgID)
		if err != nil {
			return err
		}
		after := before
		after.Required = false
		if err := org.SetSSOPolicy(ctx, tx, orgID, after); err != nil {
			return err
		}
		return ssoPolicyEvent(ctx, tx, policy, actor, orgID, before, after, SSOPolicyResult{Policy: after})
	})
}

func requireSSORevocations(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) (int, int, error) {
	sessions, err := tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE channel='cli' AND org_id=$1 AND sso_proof_id IS NULL AND revoked_at IS NULL`, orgID)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE device_codes SET status='denied' WHERE org_id=$1 AND status='approved'`, orgID); err != nil {
		return 0, 0, err
	}
	links, err := slack.RetireOrganizationLinks(ctx, tx, orgID)
	if err != nil {
		return 0, 0, err
	}
	return int(sessions.RowsAffected()), links, nil
}

// retireSSOBindings revokes what retired bindings issued: their browser SSO
// proofs, CLI sessions approved with those proofs, and their members' approved
// device codes not yet exchanged.
func retireSSOBindings(ctx context.Context, tx pgx.Tx, bindings []uuid.UUID) error {
	if len(bindings) == 0 {
		return nil
	}
	if err := sso.RevokeBindingProofs(ctx, tx, bindings); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE channel='cli' AND revoked_at IS NULL
 AND sso_proof_id IN (SELECT id FROM browser_sso_proofs WHERE binding_id=ANY($1))`, bindings); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE device_codes d SET status='denied' FROM org_sso_member_bindings b
 WHERE b.id=ANY($1) AND d.org_id=b.org_id AND d.decided_by_member_id=b.member_id AND d.status='approved'`, bindings)
	return err
}

// UnlinkSSOMember retires a member's binding with its revocation set.
func UnlinkSSOMember(ctx context.Context, database db.TxBeginner, m org.ManagingMember, member uuid.UUID) error {
	return ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, _ org.SSOPolicy) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, member); err != nil {
			return err
		}
		retired, err := sso.RetireMemberBinding(ctx, tx, m.OrgID, member)
		if err != nil {
			return err
		}
		if retired.ID == uuid.Nil() {
			return org.ErrMemberNotFound
		}
		if err := retireSSOBindings(ctx, tx, []uuid.UUID{retired.ID}); err != nil {
			return err
		}
		return audit.Append(ctx, tx, m.AuditPolicy, audit.Event{Evidence: ownerEvidence(current), OrgID: m.OrgID, Action: "sso.binding_retired", TargetKind: "member", TargetID: member,
			Details: audit.SSOBindingDetails{ConnectionID: retired.ConnectionID, IdentityID: retired.IdentityID, Subject: retired.Subject}})
	})
}

// LinkedSSOMembers lists members and their company SSO links for owners.
func LinkedSSOMembers(ctx context.Context, database db.TxBeginner, m org.ManagingMember) ([]sso.LinkedMember, error) {
	var members []sso.LinkedMember
	err := ssoOwnerTx(ctx, database, m, false, func(tx pgx.Tx, _ auth.Principal, _ org.SSOPolicy) error {
		var err error
		members, err = sso.LinkedMembers(ctx, tx, m.OrgID)
		return err
	})
	return members, err
}

// GenerateRecoveryCodes replaces the organization's recovery set and returns
// the new codes, which are shown once. Every owner is notified.
func GenerateRecoveryCodes(ctx context.Context, database db.TxBeginner, cfg Config, m org.ManagingMember) ([]string, error) {
	var codes []string
	err := ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, _ org.SSOPolicy) error {
		hashes := make([][]byte, 0, recoveryCodeCount)
		for range recoveryCodeCount {
			raw := make([]byte, 16)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			code := formatRecoveryCode(raw)
			hash, err := cfg.recoveryCodeHash(code)
			if err != nil {
				return err
			}
			codes, hashes = append(codes, code), append(hashes, hash)
		}
		if err := sso.ReplaceRecoveryCodes(ctx, tx, m.OrgID, current.MemberID, hashes); err != nil {
			return err
		}
		return recoveryEvent(ctx, tx, m.AuditPolicy, ownerEvidence(current), m.OrgID, "sso.recovery_codes_generated", "organization", m.OrgID, recoveryCodeCount)
	})
	return codes, err
}

// RecoverSSO lets an owner who signed in ordinarily enter one recovery code.
// The browser receives a proof for the organization with the usual lifetime.
// Every owner is notified.
func RecoverSSO(ctx context.Context, database db.TxBeginner, cfg Config, p auth.Principal, orgID uuid.UUID, code string) error {
	hash, err := cfg.recoveryCodeHash(code)
	if err != nil {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := org.LockSSOPolicy(ctx, tx, orgID); err != nil {
			return err
		}
		current, err := currentAccount(ctx, tx, p)
		if err != nil {
			return err
		}
		if current.IdentityID == uuid.Nil() && current.PasskeyID == uuid.Nil() {
			return ErrOrdinarySignInRequired
		}
		var member uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND user_id=$2 AND is_owner AND removed_at IS NULL AND suspended_at IS NULL FOR SHARE`, orgID, p.UserID).Scan(&member); err != nil {
			if isNoRows(err) {
				return org.ErrOwnerRoleRequired
			}
			return err
		}
		if _, err := sso.UseRecoveryCode(ctx, tx, orgID, member, p.UserID, p.SessionID, hash); errors.Is(err, sso.ErrRecoveryCodeUsed) {
			return ErrInvalidToken
		} else if err != nil {
			return err
		}
		remaining, _, err := sso.RecoveryCodes(ctx, tx, orgID)
		if err != nil {
			return err
		}
		evidence := audit.FromPrincipal(current)
		evidence.Authentication.CredentialKind, evidence.Authentication.CredentialID = "recovery_code", member
		return recoveryEvent(ctx, tx, cfg.auditPolicy, evidence, orgID, "sso.recovery_code_used", "member", member, remaining)
	})
}

func formatRecoveryCode(raw []byte) string {
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	return s[0:5] + "-" + s[5:10] + "-" + s[10:15] + "-" + s[15:20] + "-" + s[20:]
}

func (c Config) recoveryCodeHash(code string) ([]byte, error) {
	normalized := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(code)))
	if len(normalized) != 26 {
		return nil, ErrInvalidToken
	}
	return auth.MAC(c.recoveryKey, []byte("helmr.sso-recovery-code.v1\x00"), []byte(normalized))
}

func ssoConnectionEvent(ctx context.Context, tx pgx.Tx, m org.ManagingMember, current auth.Principal, action string, c sso.Connection) error {
	return audit.Append(ctx, tx, m.AuditPolicy, audit.Event{Evidence: ownerEvidence(current), OrgID: m.OrgID, Action: action, TargetKind: "sso_connection", TargetID: c.ID,
		Details: audit.SSOConnectionDetails{Protocol: c.Protocol}})
}

func ssoPolicyEvent(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, orgID uuid.UUID, before, after org.SSOPolicy, result SSOPolicyResult) error {
	return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: orgID, Action: "sso.policy_updated", TargetKind: "organization", TargetID: orgID,
		Details: audit.SSOPolicyDetails{RequiredBefore: before.Required, RequiredAfter: after.Required, SessionSecondsBefore: before.SessionSeconds, SessionSecondsAfter: after.SessionSeconds,
			RevokedCLISessions: result.RevokedCLISessions, RevokedSlackLinks: result.RevokedSlackLinks}})
}

// recoveryEvent audits a recovery-code change and notifies every owner.
func recoveryEvent(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, orgID uuid.UUID, action, targetKind string, target uuid.UUID, remaining int) error {
	if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: orgID, Action: action, TargetKind: targetKind, TargetID: target,
		Details: audit.SSORecoveryDetails{Remaining: remaining}}); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT COALESCE(u.primary_email,'') FROM org_members m JOIN users u ON u.id=m.user_id
 WHERE m.org_id=$1 AND m.is_owner AND m.removed_at IS NULL ORDER BY m.id`, orgID)
	if err != nil {
		return err
	}
	recipients, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := enqueueSecurityNotification(ctx, tx, action, recipient); err != nil {
			return err
		}
	}
	return nil
}
