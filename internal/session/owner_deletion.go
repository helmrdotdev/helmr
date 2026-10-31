package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/jackc/pgx/v5"
)

// CancelAgentSessions cancels, or with dryRun only counts, the Sessions that
// deleting an Agent ends: its Sessions that are not closed and the Sessions
// they own. Execution cleanup continues after the Agent is deleted.
func CancelAgentSessions(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, agent uuid.UUID, actor audit.Evidence, dryRun bool) (int64, error) {
	return cancelOwnedSessions(ctx, auditPolicy, tx, env, `agent_id=$2`, agent, actor, dryRun)
}

// CancelComputerSessions cancels, or with dryRun only counts, the Sessions
// that deleting a Computer ends: the Sessions placed on it that are not closed
// and the Sessions they own. It takes ownership-tree locks before the Computer
// lock, so it runs before the deletion locks the Computer.
func CancelComputerSessions(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, computerID uuid.UUID, actor audit.Evidence, dryRun bool) (int64, error) {
	return cancelOwnedSessions(ctx, auditPolicy, tx, env, `computer_id=$2`, computerID, actor, dryRun)
}

func cancelOwnedSessions(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env uuid.UUID, seed string, owner uuid.UUID, actor audit.Evidence, dryRun bool) (int64, error) {
	read := func() ([]uuid.UUID, error) {
		rows, err := tx.Query(ctx, `WITH RECURSIVE tree AS (SELECT id FROM sessions WHERE environment_id=$1 AND status<>'closed' AND `+seed+`
 UNION SELECT s.id FROM sessions s JOIN tree t ON s.parent_session_id=t.id WHERE s.environment_id=$1 AND s.status<>'closed')
 SELECT id FROM tree ORDER BY id`, env, owner)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	}
	if dryRun {
		ids, err := read()
		return int64(len(ids)), err
	}
	// The admission lock stops new Sessions from joining the set while it is
	// cancelled.
	if _, err := org.LockAdmission(ctx, tx, env); err != nil {
		return 0, err
	}
	ids, err := read()
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	locked, err := lockSessions(ctx, tx, env, ids)
	if err != nil {
		return 0, err
	}
	if err = settleTurns(ctx, auditPolicy, tx, env, ids, "cancelled", true, func(e audit.Event) error {
		e.Outcome = "cancelled"
		return auditActorFact(ctx, tx, auditPolicy, actor, e)
	}); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `UPDATE sessions SET status='closed',close_how='cancel',authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=ANY($2) AND status<>'closed' RETURNING id`, env, ids)
	if err != nil {
		return 0, err
	}
	cancelled, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	for _, id := range cancelled {
		if err = finishClose(ctx, auditPolicy, tx, env, id, "cancel"); err != nil {
			return 0, err
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, o := range locked {
		if o.computer == uuid.Nil() || seen[o.computer] {
			continue
		}
		seen[o.computer] = true
		if err = computer.RetireUnusedExecutionAdmission(ctx, tx, env, o.computer); err != nil {
			return 0, err
		}
	}
	return int64(len(cancelled)), nil
}

// ComputerSessionCancellation is the Session cancellation that Computer
// deletion requires.
func ComputerSessionCancellation(auditPolicy auditpolicy.Policy) func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, audit.Evidence, bool) (int64, error) {
	return func(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID, actor audit.Evidence, dryRun bool) (int64, error) {
		return CancelComputerSessions(ctx, auditPolicy, tx, env, computerID, actor, dryRun)
	}
}
