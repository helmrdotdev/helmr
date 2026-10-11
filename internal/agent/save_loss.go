package agent

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// The caller holds the supply and Computer locks and has irrevocably ended this
// lease's publication authority. Publication takes the same Computer lock, so a
// committed receipt is now distinguishable from definitive absence. Storage
// availability and physical absence alone cannot establish this fact.
func settleLostComputerSaves(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) error {
	_, err := tx.Exec(ctx, `UPDATE computer_saves SET status='failed',failure_evidence='publication absent after writer authority ended'
 WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch=$3 AND status IN ('requested','captured')`, env, computer, epoch)
	return err
}

// Session settlement owns its normal root -> Computer -> Session lock order.
// A source process may already be fenced and absent from the live member set.
func interruptFailedSaveTurn(ctx context.Context, database db.TxBeginner, env, session, turn uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, env, session); err != nil {
			return err
		}
		if err := expireSessionDeadlineTx(ctx, tx, env, session); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE turns t SET status='interrupted',terminal_at=clock_timestamp()
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3 AND t.status='finalizing'
 AND EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=t.environment_id AND s.turn_id=t.id AND s.status='failed')`, env, session, turn)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if err := cancelTurnAsks(ctx, tx, env, session, turn); err != nil {
			return err
		}
		if err := rejectTurnMessages(ctx, tx, env, session, turn, true, "turn_terminated"); err != nil {
			return err
		}
		return event(ctx, tx, env, session, turn, "turn.interrupted")
	})
}
