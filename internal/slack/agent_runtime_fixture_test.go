package slack

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// dispatchRuntimeTurn drives dispatch through the production runtime operations
// with the fixture Worker's host principal.
func dispatchRuntimeTurn(ctx context.Context, pool db.TxBeginner, e session.Execution) (session.TurnDispatch, error) {
	var group uuid.UUID
	var sequence int64
	if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, e.WorkerHostID).Scan(&group); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&sequence)
	}); err != nil {
		return session.TurnDispatch{}, err
	}
	host := workergroup.HostPrincipal{HostID: e.WorkerHostID, GroupID: group, Epoch: e.WorkerEpoch, HostClaimVersion: 1, GroupClaimVersion: 1}
	if sequence == 0 {
		a, err := session.AcquireRuntimeAttachment(ctx, pool, host, e)
		if err != nil {
			return session.TurnDispatch{}, err
		}
		sequence = a.Sequence
	}
	dispatch, err := session.DispatchRuntimeTurn(ctx, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, pool, host, e, sequence, fixtureConversationVerifier(true))
	if err != nil {
		return session.TurnDispatch{}, err
	}
	if dispatch == nil {
		return session.TurnDispatch{}, session.ErrNotReady
	}
	return *dispatch, nil
}

type fixtureConversationVerifier bool

func (v fixtureConversationVerifier) VerifyInputConversation(context.Context, uuid.UUID, int64, string, string, string) (bool, error) {
	return bool(v), nil
}
