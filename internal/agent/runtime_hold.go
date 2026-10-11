package agent

import (
	"context"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// RuntimeHold reports failure of this logical process, including setup before a
// Turn exists. The existing process failure fact prevents additive retry holds.
func RuntimeHold(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, reason string) error {
	if reason == "" || len(reason) > 4096 || strings.ContainsRune(reason, 0) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwnerState(ctx, tx, host, e, true); err != nil {
			return err
		}
		var recorded, stopped bool
		if err := tx.QueryRow(ctx, `SELECT failure_recorded_at IS NOT NULL,status='stopped' AND fenced_at IS NOT NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&recorded, &stopped); err != nil {
			return err
		}
		if recorded || stopped {
			return nil
		}
		return observeSessionFailure(ctx, tx, e, reason)
	}))
}
