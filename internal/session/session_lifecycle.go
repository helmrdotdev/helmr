package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type sessionLifecyclePosition struct{ environment, session uuid.UUID }

const terminalProcessPhysicallyStopped = `s.status='closed' AND p.status='stopping' AND p.fenced_at IS NULL
 AND l.fenced_at IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM computer_leases live WHERE live.environment_id=p.environment_id AND live.computer_id=p.computer_id AND live.fenced_at IS NULL)`

// mayBecomeIdle selects an open Session row s that the idle reconciler can
// start timing: nothing is running and nothing runnable is queued. Busy
// Sessions are not visited until their work settles.
const mayBecomeIdle = `NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='running')
 AND (NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued') OR ` + db.SessionHeldSQL + `)`

// RunSessionLifecycle owns deadline settlement and accepted terminal-control convergence
// independently of guest connections. The process root cancels and joins it.
func RunSessionLifecycle(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("session lifecycle database and logger are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var position sessionLifecyclePosition
	for {
		next, more, err := reconcileSessionLifecycle(ctx, auditPolicy, database, position)
		position = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Session lifecycle reconciliation failed", "error", err)
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func reconcileSessionLifecycle(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxDB, after sessionLifecyclePosition) (sessionLifecyclePosition, bool, error) {
	// Keyset traversal revisits durable intent on the next pass without letting
	// a blocked or failing root permanently exclude later Sessions from a batch.
	scanCtx, cancelScan := context.WithTimeout(ctx, 10*time.Second)
	defer cancelScan()
	candidates, err := lifecycleCandidates(scanCtx, database, after)
	if err != nil {
		return after, false, err
	}
	if len(candidates) == 0 {
		return sessionLifecyclePosition{}, false, nil
	}
	var failures []error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		after = candidate
		err := reconcileSessionStoragePressure(ctx, auditPolicy, database, candidate.environment, candidate.session)
		if err == nil {
			err = reconcileProcessStopDeadline(ctx, auditPolicy, database, candidate.environment, candidate.session)
		}
		if err == nil {
			err = expireSessionDeadline(ctx, auditPolicy, database, candidate.environment, candidate.session)
		}
		if err == nil {
			err = reconcileSessionIdle(ctx, auditPolicy, database, candidate.environment, candidate.session)
		}
		if err == nil {
			err = ReconcileSessionClosure(ctx, auditPolicy, database, candidate.environment, candidate.session)
		}
		if err == nil {
			err = reconcileTerminalSessionStop(ctx, database, candidate.environment, candidate.session)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	more := len(candidates) == 100 || after != candidates[len(candidates)-1]
	if !more {
		after = sessionLifecyclePosition{}
	}
	return after, more, errors.Join(failures...)
}

// lifecycleCandidates returns the next page of Sessions that may need
// lifecycle reconciliation after the given position.
func lifecycleCandidates(ctx context.Context, database db.TxDB, after sessionLifecyclePosition) ([]sessionLifecyclePosition, error) {
	rows, err := database.Query(ctx, `SELECT environment_id,session_id FROM (
  SELECT environment_id,id AS session_id FROM sessions s WHERE status='open' AND idle_since IS NULL AND `+mayBecomeIdle+`
  UNION
  SELECT s.environment_id,s.id FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)
   WHERE s.status='open' AND d.close_after_idle_ms IS NOT NULL
   AND NOT EXISTS(SELECT 1 FROM sessions child WHERE child.environment_id=s.environment_id AND child.parent_session_id=s.id AND child.status<>'closed')
   AND extract(epoch FROM (clock_timestamp()-s.idle_since))*1000>=d.close_after_idle_ms
  UNION
 SELECT s.environment_id,s.id FROM sessions s JOIN computer_leases l ON (l.environment_id,l.computer_id)=(s.environment_id,s.computer_id) WHERE s.status IN ('open','closing') AND l.fenced_at IS NULL AND l.storage_pressure_since<=clock_timestamp()-interval '30 seconds'
  UNION
  SELECT environment_id,session_id FROM turns WHERE status='running' AND deadline_at<=clock_timestamp()
  UNION
  SELECT environment_id,session_id FROM session_processes WHERE stop_deadline_at<=clock_timestamp() AND fenced_at IS NULL AND failure_recorded_at IS NULL
  UNION
  SELECT environment_id,id AS session_id FROM sessions s WHERE status='closing'
   AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='running')
   AND CASE WHEN EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued') THEN `+db.SessionHeldSQL+`
    ELSE NOT EXISTS(SELECT 1 FROM sessions child WHERE child.environment_id=s.environment_id AND child.parent_session_id=s.id AND child.status<>'closed') END
  UNION
  SELECT s.environment_id,s.id FROM sessions s
   JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
   JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
   WHERE `+terminalProcessPhysicallyStopped+`
 ) candidates WHERE (environment_id,session_id)>($1,$2) ORDER BY environment_id,session_id LIMIT 100`, after.environment, after.session)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (sessionLifecyclePosition, error) {
		var p sessionLifecyclePosition
		err := row.Scan(&p.environment, &p.session)
		return p, err
	})
}

// A parked process has no live guest to acknowledge a later close/cancel. The
// recorded physical fence can finish that control only while no allocation can
// contain the process, including a target allocated before restore preparation.
// Keep immutable checkpoint members so future peer restores still stop it.
func reconcileTerminalSessionStop(ctx context.Context, pool db.TxBeginner, env, session uuid.UUID) error {
	attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.RunTx(attempt, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(attempt, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		// Target allocation takes the same Computer lock. Recheck after taking
		// canonical ownership locks; discovery is not physical-stop authority.
		if _, err := lockSession(attempt, tx, env, session); err != nil {
			return err
		}
		tag, err := tx.Exec(attempt, `UPDATE session_processes p SET status='stopped',fenced_at=clock_timestamp()
 FROM sessions s,computer_leases l
 WHERE p.environment_id=$1 AND p.session_id=$2
 AND (s.environment_id,s.id)=(p.environment_id,p.session_id)
 AND (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 AND `+terminalProcessPhysicallyStopped, env, session)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return event(attempt, tx, env, session, uuid.Nil(), "session.process_stopped")
	})
}

// A wedged program cannot extend the durable stop budget through reconnection.
func reconcileProcessStopDeadline(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, env, session uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		if _, err := lockSession(ctx, tx, env, session); err != nil {
			return err
		}
		var epoch int64
		err := tx.QueryRow(ctx, `SELECT epoch FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND stop_deadline_at<=clock_timestamp() AND fenced_at IS NULL AND failure_recorded_at IS NULL FOR NO KEY UPDATE`, env, session).Scan(&epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return observeSessionFailure(ctx, auditPolicy, tx, audit.Internal(audit.ControlPlane), Execution{EnvironmentID: env, SessionID: session, ProcessEpoch: epoch}, failureStopDeadline)
	})
}
