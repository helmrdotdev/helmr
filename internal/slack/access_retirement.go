package slack

import (
	"context"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// RetireAccountAccess runs under the account root lock. A zero source retires
// every issued link; source maintenance only invalidates pending consent.
func RetireAccountAccess(ctx context.Context, tx pgx.Tx, user, source uuid.UUID) error {
	if source == uuid.Nil() {
		if _, err := tx.Exec(ctx, `UPDATE slack_user_links SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, user); err != nil {
			return err
		}
	}

	_, err := tx.Exec(ctx, `UPDATE slack_link_requests SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR identity_id=$2 OR passkey_id=$2)`, user, source)
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
