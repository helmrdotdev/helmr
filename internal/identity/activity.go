package identity

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// RecordHumanActivity refreshes only a still-live, admitted human envelope.
// Callers must distinguish explicit use from background transport maintenance.
func RecordHumanActivity(ctx context.Context, database db.TxBeginner, p auth.Principal) error {
	if p.SessionID == uuid.Nil() || p.UserID == uuid.Nil() {
		return ErrInvalidToken
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			return err
		}
		current, err := access.SessionPrincipal(ctx, tx, p.SessionID, p.OrgID)
		if err != nil {
			return err
		}
		if current.UserID != p.UserID {
			return ErrInvalidToken
		}
		tag, err := tx.Exec(ctx, `UPDATE login_sessions SET last_human_activity_at=clock_timestamp(),expires_at=clock_timestamp()+interval '30 days' WHERE id=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp() AND channel='browser'`, p.SessionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrInvalidToken
		}
		return nil
	})
}
