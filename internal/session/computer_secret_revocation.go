package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// recordComputerSecretRevocationFailure is shared by reconciliation and physical observers.
// The caller holds the Session/Computer and process locks. A prior failure marker
// survives user hold release, so a late observation cannot add another hold.
func recordComputerSecretRevocationFailure(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, e Execution) (bool, error) {
	var record bool
	if err := tx.QueryRow(ctx, `SELECT p.failure_recorded_at IS NULL AND s.status IN ('open','closing') AND EXISTS(
 SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) r WHERE r.environment_id=p.environment_id AND r.computer_id=p.computer_id)
 FROM session_processes p JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&record); err != nil {
		return false, err
	}
	if !record {
		return false, nil
	}
	if err := recordSessionFailure(ctx, auditPolicy, tx, audit.System("secret-revocation"), e, "Computer exposed to a revoked Secret"); err != nil {
		return false, err
	}
	_, err := tx.Exec(ctx, `UPDATE session_processes SET failure_recorded_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
	return err == nil, err
}

// ReconcileComputerSecretRevocation interrupts exposed peers and preserves queued
// input for explicit disposition. It does not cancel descendants on other Computers or erase
// recorded results. Only existing exact lease stop evidence may fence a process.
func ReconcileComputerSecretRevocation(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, env, computerID uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		// Include Sessions with queued work but no process. Admission must acquire
		// this same Computer lock; revalidate discovery after the complete owner set.
		read := func() ([]uuid.UUID, error) {
			rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2 ORDER BY id`, env, computerID)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		before, err := read()
		if err != nil {
			return err
		}
		if _, err = LockSessionsAndComputers(ctx, tx, env, before, []uuid.UUID{computerID}); err != nil {
			return err
		}
		after, err := read()
		if err != nil {
			return err
		}
		if !slices.Equal(before, after) {
			return ErrNotReady
		}
		if err = computer.RequireSecretsNotRevoked(ctx, tx, env, computerID); err == nil {
			return nil
		} else if !errors.Is(err, computer.ErrDenied) {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT session_id,epoch,computer_lease_epoch FROM session_processes WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL ORDER BY session_id FOR NO KEY UPDATE`, env, computerID)
		if err != nil {
			return err
		}
		members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Execution, error) {
			e := Execution{EnvironmentID: env}
			err := row.Scan(&e.SessionID, &e.ProcessEpoch, &e.LeaseEpoch)
			return e, err
		})
		if err != nil {
			return err
		}
		for _, e := range members {
			if _, err = recordComputerSecretRevocationFailure(ctx, auditPolicy, tx, e); err != nil {
				return err
			}
			if err = interruptRunningSessionTurn(ctx, auditPolicy, tx, audit.System("secret-revocation"), e); err != nil {
				return err
			}
			// A hibernated logical process may retain an already-fenced source lease.
			// Reuse that exact evidence after its continuation becomes ineligible.
			changed, err := tx.Exec(ctx, `UPDATE session_processes p SET status=CASE WHEN l.fenced_at IS NOT NULL THEN 'lost' WHEN p.status IN ('starting','ready') THEN 'stopping' ELSE p.status END,
 fenced_at=COALESCE(p.fenced_at,l.fenced_at)
 FROM computer_leases l WHERE (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 AND p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND (p.status IN ('starting','ready') OR l.fenced_at IS NOT NULL)`, env, e.SessionID, e.ProcessEpoch)
			if err != nil {
				return err
			}
			if changed.RowsAffected() > 0 {
				if err = notifySessionsChanged(ctx, tx, env, e.SessionID); err != nil {
					return err
				}
			}
		}
		// Queued input remains admitted and inspectable. Image eligibility prevents
		// dispatch; only an explicit control operation may cancel that input.
		// Protocol state stays intact so an acquiring target can still receive
		// shutdown and report actual stop. Checkpoint eligibility follows lineage.
		return nil
	}))
}
