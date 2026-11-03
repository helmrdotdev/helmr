package slack

import (
	"context"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// RetireAccountAccess runs under the account root lock and retires every Slack
// link and pending link request of the account.
func RetireAccountAccess(ctx context.Context, tx pgx.Tx, user uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE slack_user_links SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, user); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE slack_link_requests SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, user)
	return err
}

// RetireMemberAccess runs after locking the exact membership and account roots.
func RetireMemberAccess(ctx context.Context, tx pgx.Tx, org, member uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE slack_user_links SET revoked_at=clock_timestamp() WHERE org_id=$1 AND member_id=$2 AND revoked_at IS NULL`, org, member); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE slack_link_requests SET revoked_at=clock_timestamp() WHERE org_id=$1 AND member_id=$2 AND revoked_at IS NULL`, org, member)
	return err
}

// RetireMemberLinks retires the Slack links and pending link requests of the
// organization's members other than its owners, when it starts requiring SSO,
// and returns how many links it retired.
func RetireMemberLinks(ctx context.Context, tx pgx.Tx, org uuid.UUID) (int, error) {
	tag, err := tx.Exec(ctx, `UPDATE slack_user_links l SET revoked_at=clock_timestamp() FROM org_members m
 WHERE l.org_id=$1 AND l.revoked_at IS NULL AND m.id=l.member_id AND m.org_id=l.org_id AND NOT m.is_owner`, org)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE slack_link_requests r SET revoked_at=clock_timestamp() FROM org_members m
 WHERE r.org_id=$1 AND r.revoked_at IS NULL AND m.id=r.member_id AND m.org_id=r.org_id AND NOT m.is_owner`, org)
	return int(tag.RowsAffected()), err
}
