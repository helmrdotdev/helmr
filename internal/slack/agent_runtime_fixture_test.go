package slack

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// dispatchRuntimeTurn drives dispatch through the production runtime operations
// with the fixture Worker's host principal.
func dispatchRuntimeTurn(ctx context.Context, pool db.TxBeginner, e agent.Execution) (agent.TurnDispatch, error) {
	var group uuid.UUID
	var sequence int64
	if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, e.WorkerHostID).Scan(&group); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&sequence)
	}); err != nil {
		return agent.TurnDispatch{}, err
	}
	host := workergroup.HostPrincipal{HostID: e.WorkerHostID, GroupID: group, Epoch: e.WorkerEpoch, HostClaimVersion: 1, GroupClaimVersion: 1}
	if sequence == 0 {
		a, err := agent.AcquireRuntimeAttachment(ctx, pool, host, e)
		if err != nil {
			return agent.TurnDispatch{}, err
		}
		sequence = a.Sequence
	}
	dispatch, err := agent.DispatchRuntimeTurn(ctx, pool, host, e, sequence)
	if err != nil {
		return agent.TurnDispatch{}, err
	}
	if dispatch == nil {
		return agent.TurnDispatch{}, agent.ErrNotReady
	}
	return *dispatch, nil
}
