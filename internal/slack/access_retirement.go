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
