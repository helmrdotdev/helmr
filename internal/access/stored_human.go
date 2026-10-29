package access

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"uuid"
)

// HumanColumns is the immutable source snapshot stored by operation owners.
const HumanColumns = `human_user_id,human_org_id,human_member_id,human_session_id,human_slack_link_id,human_identity_id,human_passkey_id,human_access_epoch,human_authenticated_at`

func HumanValues(p auth.Principal) []any {
	if p.UserID == uuid.Nil() {
		return []any{nil, nil, nil, nil, nil, nil, nil, nil, nil}
	}
	return []any{p.UserID, p.OrgID, p.MemberID, optionalID(p.SessionID), optionalID(p.SlackLinkID), optionalID(p.IdentityID), optionalID(p.PasskeyID), p.AccessEpoch, p.AuthenticatedAt}
}

func ReadHuman(row pgx.Row) (auth.Principal, error) {
	var user, org, member, session, link, identity, key pgtype.UUID
	var epoch pgtype.Int8
	var at pgtype.Timestamptz
	if err := row.Scan(&user, &org, &member, &session, &link, &identity, &key, &epoch, &at); err != nil {
		return auth.Principal{}, err
	}
	p := auth.Principal{Kind: auth.PrincipalKindSession, UserID: nullableID(user), OrgID: nullableID(org), MemberID: nullableID(member), SessionID: nullableID(session), SlackLinkID: nullableID(link), IdentityID: nullableID(identity), PasskeyID: nullableID(key), AccessEpoch: epoch.Int64, AuthenticatedAt: at.Time}
	if p.UserID == uuid.Nil() {
		return auth.Principal{}, ErrDenied
	}
	return p, nil
}

func HumanInEnvironment(ctx context.Context, q db.DBTX, p auth.Principal, env uuid.UUID) (auth.Principal, error) {
	var org, project uuid.UUID
	if err := q.QueryRow(ctx, `SELECT org_id,project_id FROM environments WHERE id=$1`, env).Scan(&org, &project); err != nil {
		return p, humanReadError(err)
	}
	return Current(ctx, q, p, auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: env.String()})
}
