package identity

import (
	"context"
	"errors"
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

var ErrSSOSessionLifetime = errors.New("the single sign-on session lifetime must be between 1 and 30 days")

// ssoOwnerTx runs an SSO administration change under the organization row
// lock, for a live owner, with recent proof when recent is set.
// A change (recent set) takes the organization row lock that issuing paths
// share, and only a browser may make it; reads take no lock.
func ssoOwnerTx(ctx context.Context, database db.TxBeginner, m org.ManagingMember, recent bool, fn func(tx pgx.Tx, current auth.Principal, policy org.SSOPolicy) error) error {
	if m.AuditPolicy.Validate() != nil {
		return errors.New("audit policy is required")
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		read := org.ReadSSOPolicy
		if recent {
			read = org.LockSSOPolicy
		}
		policy, err := read(ctx, tx, m.OrgID)
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
			if current.Channel != "browser" {
				return ErrAccountProofRequired
			}
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
	Policy        org.SSOPolicy
	Active        *sso.Connection
	Candidate     *sso.Connection
	MembersLinked int
	MembersTotal  int
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
		return nil
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
// test signed in through it.
func CompleteSSOTest(ctx context.Context, database db.TxBeginner, cfg Config, store *sso.Store, p auth.Principal, orgID, member, id uuid.UUID) error {
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
		if c.State != "candidate" {
			return sso.ErrConnectionNotFound
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
		previous, links, err := sso.RetireActive(ctx, tx, m.OrgID)
		if err != nil {
			return err
		}
		if err := retireSSOLinks(ctx, tx, links); err != nil {
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
			_, links, err := sso.RetireActive(ctx, tx, m.OrgID)
			if err != nil {
				return err
			}
			if err := retireSSOLinks(ctx, tx, links); err != nil {
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

// SSOPolicyChange sets the required switch, the session lifetime, or both; a
// nil field is left unchanged.
type SSOPolicyChange struct {
	Required       *bool
	SessionSeconds *int32
}

// SSOPolicyResult reports what requiring SSO revoked.
type SSOPolicyResult struct {
	Policy             org.SSOPolicy
	RevokedCLISessions int
	RevokedSlackLinks  int
}

// UpdateSSOPolicy changes the policy. Requiring SSO needs an active connection
// and an enforcing owner currently signed in through it, and revokes once the
// organization's CLI sessions not approved through SSO, approved device codes
// not yet exchanged, Slack links and pending Slack link requests. Returning to
// optional revokes nothing.
func UpdateSSOPolicy(ctx context.Context, database db.TxBeginner, m org.ManagingMember, change SSOPolicyChange) (SSOPolicyResult, error) {
	var result SSOPolicyResult
	err := ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, before org.SSOPolicy) error {
		after := before
		if change.SessionSeconds != nil {
			if *change.SessionSeconds < 86400 || *change.SessionSeconds > 2592000 {
				return ErrSSOSessionLifetime
			}
			after.SessionSeconds = *change.SessionSeconds
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
			// The enforcing owner proves the connection works for them.
			signedIn, err := sso.LinkProof(ctx, tx, current.SSOProofID)
			if err != nil {
				return err
			}
			if !signedIn {
				return sso.ErrEnforcerNotLinked
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

func requireSSORevocations(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) (int, int, error) {
	// Owners keep their ordinary sign-in, so their CLI sessions and Slack
	// links stay.
	sessions, err := tx.Exec(ctx, `UPDATE login_sessions s SET revoked_at=clock_timestamp() FROM org_members m
 WHERE s.channel='cli' AND s.org_id=$1 AND s.sso_proof_id IS NULL AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND m.id=s.member_id AND m.org_id=s.org_id AND NOT m.is_owner`, orgID)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE device_codes d SET status='denied' FROM org_members m
 WHERE d.org_id=$1 AND d.status='approved' AND m.id=d.decided_by_member_id AND m.org_id=d.org_id AND NOT m.is_owner`, orgID); err != nil {
		return 0, 0, err
	}
	links, err := slack.RetireMemberLinks(ctx, tx, orgID)
	if err != nil {
		return 0, 0, err
	}
	return int(sessions.RowsAffected()), links, nil
}

// retireSSOLinks revokes what retired links issued: their browser SSO
// proofs, CLI sessions approved with those proofs, and their members' approved
// device codes not yet exchanged.
func retireSSOLinks(ctx context.Context, tx pgx.Tx, links []uuid.UUID) error {
	if len(links) == 0 {
		return nil
	}
	if err := sso.RevokeLinkProofs(ctx, tx, links); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE channel='cli' AND revoked_at IS NULL
 AND sso_proof_id IN (SELECT id FROM sso_proofs WHERE link_id=ANY($1))`, links); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE device_codes d SET status='denied' FROM sso_member_links b
 WHERE b.id=ANY($1) AND d.org_id=b.org_id AND d.decided_by_member_id=b.member_id AND d.status='approved'`, links)
	return err
}

// UnlinkSSOMember retires a member's link with its revocation set.
func UnlinkSSOMember(ctx context.Context, database db.TxBeginner, m org.ManagingMember, member uuid.UUID) error {
	return ssoOwnerTx(ctx, database, m, true, func(tx pgx.Tx, current auth.Principal, _ org.SSOPolicy) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND id=$2 FOR UPDATE`, m.OrgID, member); err != nil {
			return err
		}
		retired, err := sso.RetireMemberLink(ctx, tx, m.OrgID, member)
		if err != nil {
			return err
		}
		if retired.ID == uuid.Nil() {
			return org.ErrMemberNotFound
		}
		if err := retireSSOLinks(ctx, tx, []uuid.UUID{retired.ID}); err != nil {
			return err
		}
		return audit.Append(ctx, tx, m.AuditPolicy, audit.Event{Evidence: ownerEvidence(current), OrgID: m.OrgID, Action: "sso.member_unlinked", TargetKind: "member", TargetID: member,
			Details: audit.SSOLinkDetails{ConnectionID: retired.ConnectionID, IdentityID: retired.IdentityID, Subject: retired.Subject}})
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

func ssoConnectionEvent(ctx context.Context, tx pgx.Tx, m org.ManagingMember, current auth.Principal, action string, c sso.Connection) error {
	return audit.Append(ctx, tx, m.AuditPolicy, audit.Event{Evidence: ownerEvidence(current), OrgID: m.OrgID, Action: action, TargetKind: "sso_connection", TargetID: c.ID,
		Details: audit.SSOConnectionDetails{Protocol: c.Protocol}})
}

func ssoPolicyEvent(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Evidence, orgID uuid.UUID, before, after org.SSOPolicy, result SSOPolicyResult) error {
	return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: orgID, Action: "sso.policy_updated", TargetKind: "organization", TargetID: orgID,
		Details: audit.SSOPolicyDetails{RequiredBefore: before.Required, RequiredAfter: after.Required, SessionSecondsBefore: before.SessionSeconds, SessionSecondsAfter: after.SessionSeconds,
			RevokedCLISessions: result.RevokedCLISessions, RevokedSlackLinks: result.RevokedSlackLinks}})
}

// UpdateMemberOwnership changes ownership. Owners keep their ordinary sign-in
// under required SSO, so a former owner's Slack links, issued without SSO,
// retire with the ownership; their browser and CLI sessions are checked at use.
func UpdateMemberOwnership(ctx context.Context, database db.TxBeginner, manager org.ManagingMember, target uuid.UUID, value, expected bool) (org.UpdatedMember, error) {
	var result org.UpdatedMember
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var err error
		if result, err = org.UpdateMemberOwnershipTx(ctx, tx, manager, target, value, expected); err != nil {
			return err
		}
		if value || !expected {
			return nil
		}
		policy, err := org.ReadSSOPolicy(ctx, tx, manager.OrgID)
		if err != nil || !policy.Required {
			return err
		}
		return slack.RetireMemberAccess(ctx, tx, manager.OrgID, target)
	})
	return result, err
}
