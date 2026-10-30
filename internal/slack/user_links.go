package slack

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

var ErrUserLinkDenied = errors.New("the Slack identity link is unavailable for this account")
var ErrUserLinkConflict = errors.New("this Slack identity or Helmr account already has a different link in this workspace; unlink that account first")

type UserLink struct {
	TeamID      string    `json:"team_id"`
	SlackUserID string    `json:"slack_user_id"`
	LinkedAt    time.Time `json:"linked_at"`
}
type LinkWorkspace struct {
	InstallationID uuid.UUID `json:"installation_id"`
	TeamID         string    `json:"team_id"`
	WorkspaceName  *string   `json:"workspace_name"`
}

func lockLinkUser(ctx context.Context, tx pgx.Tx, org, user uuid.UUID) (uuid.UUID, error) {
	var member uuid.UUID
	err := tx.QueryRow(ctx, `SELECT m.id FROM org_members m WHERE m.org_id=$1 AND m.user_id=$2 AND m.removed_at IS NULL AND m.suspended_at IS NULL FOR SHARE OF m`, org, user).Scan(&member)
	if errors.Is(err, pgx.ErrNoRows) {
		return member, ErrUserLinkDenied
	}
	return member, err
}

// Visibility follows Session actions or the Agent's connection authority.
const linkWorkspaceAccess = `EXISTS (
 SELECT 1 FROM slack_connections c JOIN (` + access.MemberPermissionGrantsSQL + `) g
 ON g.org_id=i.org_id AND g.environment_id=c.environment_id
 WHERE c.slack_installation_id=i.id AND c.revoked_at IS NULL
 AND g.permission IN ('sessions.create','sessions.read','sessions.send','sessions.answer','sessions.resume','sessions.interrupt','sessions.cancel','sessions.close','agent_connections.read','agent_connections.configure','agent_connections.delete')
 AND (g.agent_id IS NULL OR g.agent_id=c.agent_id))`

func CheckLinkWorkspace(ctx context.Context, pool db.TxBeginner, org, user, installation uuid.UUID) (LinkWorkspace, error) {
	var v LinkWorkspace
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		member, err := lockLinkUser(ctx, tx, org, user)
		if err != nil {
			return err
		}
		// Linking alone grants no org or publication authority. Read the connection
		// without acquiring admission's installation gate while holding user locks.
		err = tx.QueryRow(ctx, `SELECT i.id,i.team_id,i.workspace_name FROM slack_installations i WHERE i.id=$4 AND i.org_id=$2 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND `+linkWorkspaceAccess, auth.AgentPermissionIDs(), org, member, installation).Scan(&v.InstallationID, &v.TeamID, &v.WorkspaceName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserLinkDenied
		}
		return err
	})
	return v, err
}
func ListLinkWorkspaces(ctx context.Context, pool db.TxBeginner, org, user, before uuid.UUID, limit int) ([]LinkWorkspace, error) {
	result := []LinkWorkspace{}
	if limit < 1 || limit > 500 {
		return nil, ErrUserLinkDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		member, err := lockLinkUser(ctx, tx, org, user)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT i.id,i.team_id,i.workspace_name FROM slack_installations i WHERE i.org_id=$2 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND ($4::uuid='00000000-0000-0000-0000-000000000000' OR i.id<$4) AND `+linkWorkspaceAccess+` ORDER BY i.id DESC LIMIT $5`, auth.AgentPermissionIDs(), org, member, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v LinkWorkspace
			if err = rows.Scan(&v.InstallationID, &v.TeamID, &v.WorkspaceName); err != nil {
				return err
			}
			result = append(result, v)
		}
		return rows.Err()
	})
	return result, err
}
func ListUserLinks(ctx context.Context, pool db.TxBeginner, org, user uuid.UUID, after string, limit int) ([]UserLink, error) {
	result := []UserLink{}
	if limit < 1 || limit > 500 || len(after) > 100 {
		return nil, ErrUserLinkDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := lockLinkUser(ctx, tx, org, user); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT team_id,slack_user_id,linked_at FROM slack_user_links WHERE user_id=$1 AND team_id>$2 AND org_id=$4 AND revoked_at IS NULL ORDER BY team_id LIMIT $3`, user, after, limit, org)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v UserLink
			if err = rows.Scan(&v.TeamID, &v.SlackUserID, &v.LinkedAt); err != nil {
				return err
			}
			result = append(result, v)
		}
		return rows.Err()
	})
	return result, err
}

// LinkUser is called only after OpenID proof and explicit browser confirmation.
// Uniqueness never performs a merge or replacement, and exact retries preserve
// the original link time used to reject older Slack gestures.
func LinkUser(ctx context.Context, pool db.TxBeginner, policy auditpolicy.Policy, actor audit.Evidence, org, user, installation uuid.UUID, identity UserIdentity, consent LinkConsent) error {
	if actor.Actor.UserID != user || user == uuid.Nil() {
		return ErrUserLinkDenied
	}
	actor.Authorization = audit.NotEvaluated()

	if identity.TeamID == "" || identity.SlackUserID == "" || len(identity.TeamID) > 100 || len(identity.SlackUserID) > 100 {
		return ErrUserLinkDenied
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		current, err := linkConsentPrincipal(ctx, tx, actor, org, user)
		if err != nil {
			return err
		}
		member := current.MemberID
		// A new proof cannot substitute for the source shown at consent.
		var consumed uuid.UUID
		err = tx.QueryRow(ctx, `UPDATE slack_link_requests SET consumed_at=clock_timestamp()
 WHERE id=$1 AND browser_hash=$2 AND browser_session_id=$3 AND installation_id=$4
 AND org_id=$5 AND member_id=$6 AND user_id=$7 AND team_id=$8 AND slack_user_id=$9
 AND identity_id IS NOT DISTINCT FROM $10
 AND passkey_id IS NOT DISTINCT FROM $11
 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp() RETURNING id`,
			consent.ID, consent.BrowserHash, current.SessionID, installation, org, member, user, identity.TeamID, identity.SlackUserID,
			optionalLinkID(current.IdentityID), optionalLinkID(current.PasskeyID)).Scan(&consumed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserLinkDenied
		}
		if err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_installations i WHERE i.id=$4 AND i.org_id=$2 AND i.team_id=$5 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND `+linkWorkspaceAccess+`)`, auth.AgentPermissionIDs(), org, member, installation, identity.TeamID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrUserLinkDenied
		}
		tag, err := tx.Exec(ctx, `INSERT INTO slack_user_links(id,team_id,slack_user_id,user_id,installation_id,org_id,member_id,access_epoch,identity_id,passkey_id,authenticated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, uuid.NewV7(), identity.TeamID, identity.SlackUserID, user, installation, org, member, current.AccessEpoch, optionalLinkID(current.IdentityID), optionalLinkID(current.PasskeyID), current.AuthenticatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: org, Action: "slack_user.linked", TargetKind: "slack_user_link", TargetUserID: user, SlackTeamID: identity.TeamID, SlackUserID: identity.SlackUserID})
		}
		var existing uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM slack_user_links WHERE team_id=$1 AND slack_user_id=$2 AND user_id=$3 AND installation_id=$4 AND revoked_at IS NULL`, identity.TeamID, identity.SlackUserID, user, installation).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserLinkConflict
		}
		if err != nil {
			return err
		}
		if _, err = access.SlackPrincipal(ctx, tx, existing); errors.Is(err, access.ErrDenied) {
			return ErrUserLinkConflict
		}
		if err != nil {
			return err
		}

		return nil
	})
}

// An exact Slack user ID prevents a stale settings page from removing a newer
// replacement link for the same workspace. Historical admitted requests remain.
func UnlinkUser(ctx context.Context, pool db.TxBeginner, policy auditpolicy.Policy, actor audit.Evidence, org, user uuid.UUID, team, slackUser string) error {
	if actor.Actor.UserID != user || user == uuid.Nil() {
		return ErrUserLinkDenied
	}
	actor.Authorization = audit.NotEvaluated()

	if team == "" || slackUser == "" || len(team) > 100 || len(slackUser) > 100 {
		return ErrUserLinkDenied
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := linkConsentPrincipal(ctx, tx, actor, org, user); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE slack_user_links SET revoked_at=clock_timestamp() WHERE team_id=$1 AND slack_user_id=$2 AND user_id=$3 AND org_id=$4 AND revoked_at IS NULL`, team, slackUser, user, org)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: org, Action: "slack_user.unlinked", TargetKind: "slack_user_link", TargetUserID: user, SlackTeamID: team, SlackUserID: slackUser})
	})
}

// Consent is issued from a live browser proof under the same account root lock
// as source retirement and revoke-all. The resulting link is independent of it.
func linkConsentPrincipal(ctx context.Context, tx pgx.Tx, e audit.Evidence, org, user uuid.UUID) (auth.Principal, error) {
	if e.Authentication.CredentialKind != "login_session" || e.Authentication.CredentialID == uuid.Nil() {
		return auth.Principal{}, ErrUserLinkDenied
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, user).Scan(&locked); err != nil {
		return auth.Principal{}, err
	}
	p, err := access.SessionPrincipal(ctx, tx, e.Authentication.CredentialID, org)
	if errors.Is(err, access.ErrDenied) {
		return p, ErrUserLinkDenied
	}
	if err != nil {
		return p, err
	}
	if p.UserID != user || p.Channel != "browser" || !access.Recent(p, time.Now()) {
		return p, ErrUserLinkDenied
	}
	return p, nil
}
func optionalLinkID(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return pgvalue.UUID(id)
}

// LinkConsent is a one-use confirmation bound to an exact browser and source.
type LinkConsent struct {
	ID          uuid.UUID
	BrowserHash []byte
}

func PrepareUserLink(ctx context.Context, pool db.TxBeginner, actor audit.Evidence, org, user, installation uuid.UUID, identity UserIdentity, binding []byte) (uuid.UUID, error) {
	id := uuid.NewV7()
	if len(binding) != 32 || actor.Actor.UserID != user || identity.TeamID == "" || identity.SlackUserID == "" {
		return uuid.Nil(), ErrUserLinkDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		p, err := linkConsentPrincipal(ctx, tx, actor, org, user)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO slack_link_requests(id,installation_id,org_id,team_id,slack_user_id,browser_hash,browser_session_id,user_id,member_id,identity_id,passkey_id,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,clock_timestamp()+interval '10 minutes')`, id, installation, org, identity.TeamID, identity.SlackUserID, binding, p.SessionID, user, p.MemberID, optionalLinkID(p.IdentityID), optionalLinkID(p.PasskeyID))
		return err
	})
	return id, err
}
