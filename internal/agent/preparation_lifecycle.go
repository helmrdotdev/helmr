package agent

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// expirePreparation makes deadline/authority loss visible without asserting that
// the physical writer stopped. A never-claimed attempt has no physical blocker.
func expirePreparation(ctx context.Context, pool db.TxBeginner, env, id uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var host, group *uuid.UUID
		var spec uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT p.preparation_spec_id,p.worker_host_id,h.worker_group_id FROM computer_preparations p LEFT JOIN worker_hosts h ON h.id=p.worker_host_id WHERE p.environment_id=$1 AND p.id=$2`, env, id).Scan(&spec, &host, &group); err != nil {
			return err
		}
		if host != nil {
			if _, err := tx.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, *group); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT id FROM worker_hosts WHERE id=$1 FOR SHARE`, *host); err != nil {
				return err
			}
		}
		if err := lockPreparationSpec(ctx, tx, env, spec); err != nil {
			return err
		}
		var same, deadline, unavailable bool
		var status string
		if err := tx.QueryRow(ctx, `SELECT p.worker_host_id IS NOT DISTINCT FROM $3::uuid,p.status,p.deadline_at<=clock_timestamp(),
    COALESCE(p.executor_expires_at<=clock_timestamp() OR h.current_epoch IS DISTINCT FROM p.worker_epoch OR h.status NOT IN ('active','draining'),false)
   FROM computer_preparations p LEFT JOIN worker_hosts h ON h.id=p.worker_host_id WHERE p.environment_id=$1 AND p.id=$2 FOR NO KEY UPDATE OF p`, env, id, host).Scan(&same, &status, &deadline, &unavailable); err != nil {
			return err
		}
		if !same {
			return ErrNotReady
		} // Claim changed the discovered supply set; retry from its first lock.
		if status != "queued" && status != "running" {
			return nil
		}
		if !deadline && !(host != nil && unavailable) {
			return nil
		}
		code := "executor_lost"
		if deadline {
			code = "deadline_exceeded"
		}
		_, err := tx.Exec(ctx, `UPDATE computer_preparations SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='failed',error_code=$3 WHERE environment_id=$1 AND id=$2`, env, id, code)
		return err
	}))
}

// reconcilePreparationRevocation closes logical execution permission. It does
// not release the unfenced-writer index or claim that the host has stopped.
func reconcilePreparationRevocation(ctx context.Context, pool db.TxBeginner, env, id uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		p, err := readPreparation(ctx, tx, env, id)
		if err != nil {
			return err
		}
		// Lock exposures' Secrets before the spec. The attempt's exposure set can
		// only grow under these same Secrets and spec locks, then is immutable.
		rows, err := tx.Query(ctx, `SELECT s.id FROM secrets s JOIN secret_exposures x ON (x.environment_id,x.secret_id)=(s.environment_id,s.id)
   WHERE x.environment_id=$1 AND x.preparation_id=$2 ORDER BY s.id FOR SHARE OF s`, env, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var locked uuid.UUID
			if err = rows.Scan(&locked); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if err = lockPreparationSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_preparations p SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='failed',error_code='secret_revoked'
   WHERE p.environment_id=$1 AND p.id=$2 AND p.status IN ('queued','running') AND EXISTS(
    SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
    WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id AND s.status='revoked')`, env, id)
		return err
	}))
}

type stoppedPreparation struct {
	env, spec, id, instance uuid.UUID
	hostEpoch               int64
}

func hostPreparations(ctx context.Context, tx pgx.Tx, host uuid.UUID, beforeEpoch *int64, quarantined []uuid.UUID) ([]stoppedPreparation, error) {
	rows, err := tx.Query(ctx, `SELECT environment_id,preparation_spec_id,id,instance_id,worker_epoch FROM computer_preparations
  WHERE worker_host_id=$1 AND fenced_at IS NULL AND ($2::bigint IS NULL OR worker_epoch<$2) AND NOT(instance_id=ANY($3::uuid[]))
  ORDER BY environment_id,preparation_spec_id,id`, host, beforeEpoch, quarantined)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (stoppedPreparation, error) {
		var p stoppedPreparation
		err := row.Scan(&p.env, &p.spec, &p.id, &p.instance, &p.hostEpoch)
		return p, err
	})
}

// Called with provider-confirmation supply locks before any Session/Computer
// lock, so all specs precede the full Computer set in the shared transaction.
func lockAbsentHostPreparations(ctx context.Context, tx pgx.Tx, host uuid.UUID) ([]stoppedPreparation, error) {
	attempts, err := hostPreparations(ctx, tx, host, nil, []uuid.UUID{})
	if err != nil {
		return nil, err
	}
	for _, p := range attempts {
		if err = lockPreparationSpec(ctx, tx, p.env, p.spec); err != nil {
			return nil, err
		}
	}
	return attempts, nil
}
func observeRecoveredHostPreparations(ctx context.Context, database db.TxDB, host workergroup.HostPrincipal, quarantined []uuid.UUID) error {
	var attempts []stoppedPreparation
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockRecoveringComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var err error
		attempts, err = hostPreparations(ctx, tx, host.HostID, &host.Epoch, quarantined)
		return err
	})
	if err != nil {
		return err
	}
	var pending bool
	for _, p := range attempts {
		err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
				return err
			}
			if err := lockRecoveringComputerHost(ctx, tx, host); err != nil {
				return err
			}
			if err := lockPreparationSpec(ctx, tx, p.env, p.spec); err != nil {
				return err
			}
			return recordPreparationStopped(ctx, tx, p.env, p.id, "successor worker completed local VM inventory and physical cleanup")
		})
		if err != nil {
			var lock *pgconn.PgError
			if errors.Is(err, ErrNotReady) || (errors.As(err, &lock) && lock.Code == "55P03") {
				pending = true
				continue
			}
			return err
		}
	}
	if pending {
		return ErrNotReady
	}
	return nil
}

// expirePreparationWaiter fails only never-dispatched work. Computer storage and
// the executor's physical lifetime remain owned by their separate reconcilers.
func expirePreparationWaiter(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var spec *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT preparation_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&spec); err != nil {
			return err
		}
		if spec == nil {
			return nil
		}
		if err := lockPreparationSpec(ctx, tx, env, *spec); err != nil {
			return err
		}
		read := func() ([]uuid.UUID, error) {
			rows, err := tx.Query(ctx, `WITH roots AS (SELECT DISTINCT root_session_id AS id FROM sessions WHERE environment_id=$1 AND computer_id=$2)
    SELECT s.id FROM sessions s JOIN roots r ON r.id=s.root_session_id WHERE s.environment_id=$1 ORDER BY s.id`, env, computer)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		before, err := read()
		if err != nil {
			return err
		}
		if _, err = lockSessionsAndComputers(ctx, tx, env, before, []uuid.UUID{computer}); err != nil {
			return err
		}
		after, err := read()
		if err != nil {
			return err
		}
		if !slices.Equal(before, after) {
			return ErrNotReady
		}
		var due bool
		if err = tx.QueryRow(ctx, `SELECT initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at<=clock_timestamp()
   FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		var dispatched bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND computer_id=$2 AND started_at IS NOT NULL)`, env, computer).Scan(&dispatched); err != nil {
			return err
		}
		if dispatched {
			return ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE computers SET preparation_failed_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, computer); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp()
   WHERE environment_id=$1 AND computer_id=$2 AND status='queued' RETURNING id,session_id`, env, computer)
		if err != nil {
			return err
		}
		type failed struct{ turn, session uuid.UUID }
		failedTurns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (failed, error) {
			var f failed
			err := row.Scan(&f.turn, &f.session)
			return f, err
		})
		if err != nil {
			return err
		}
		for _, turn := range failedTurns {
			if err = eventData(ctx, tx, env, turn.session, turn.turn, "turn.failed", []byte(`{"code":"preparation_deadline"}`)); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE sessions SET status='closing' WHERE environment_id=$1 AND computer_id=$2 AND status='open'`, env, computer); err != nil {
			return err
		}
		return drainClosingSessions(ctx, tx, env, after)
	}))
}

type preparationLifecyclePosition struct {
	environment, id uuid.UUID
	kind            string
}

// RunPreparationLifecycle owns due waiter settlement, attempt expiry and revoked
// exposure reconciliation independently of guest connectivity.
func RunPreparationLifecycle(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("preparation lifecycle database and logger are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var after preparationLifecyclePosition
	for {
		next, more, err := reconcilePreparationLifecycle(ctx, database, after)
		after = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "preparation lifecycle reconciliation failed", "error", err)
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
func reconcilePreparationLifecycle(ctx context.Context, database db.TxDB, after preparationLifecyclePosition) (preparationLifecyclePosition, bool, error) {
	scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := database.Query(scanCtx, `WITH candidates AS (
  SELECT environment_id,id,'waiter'::text AS kind FROM computers WHERE initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at<=statement_timestamp()
  UNION
  SELECT c.environment_id,c.id,'unattached' FROM computers c JOIN environments e ON e.id=c.environment_id WHERE e.retired_at IS NULL AND c.initial_root_digest IS NULL AND preparation_id IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at>statement_timestamp()
  UNION
  SELECT c.environment_id,c.id,'image' FROM computers c JOIN computer_preparations p ON (p.environment_id,p.id)=(c.environment_id,c.preparation_id)
   WHERE c.initial_root_digest IS NULL AND p.status='succeeded' AND c.preparation_failed_at IS NULL AND c.deleted_at IS NULL AND c.preparation_deadline_at>statement_timestamp()
  UNION
  SELECT revoked.environment_id,revoked.computer_id,'image_revocation' FROM computer_secret_revocations revoked
   JOIN session_processes p ON (p.environment_id,p.computer_id)=(revoked.environment_id,revoked.computer_id)
   WHERE p.fenced_at IS NULL AND (p.status IN ('starting','ready') OR EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=p.environment_id AND t.session_id=p.session_id AND t.process_epoch=p.epoch AND t.status='running'))
  UNION
  SELECT environment_id,id,'attempt' FROM computer_preparations WHERE status IN ('queued','running') AND deadline_at<=statement_timestamp()
  UNION
  SELECT environment_id,id,'attempt' FROM computer_preparations WHERE status='running' AND executor_expires_at<=statement_timestamp()
  UNION
  SELECT p.environment_id,p.id,'attempt' FROM worker_hosts h JOIN computer_preparations p ON p.worker_host_id=h.id
   WHERE p.status='running' AND (h.current_epoch IS DISTINCT FROM p.worker_epoch OR h.status NOT IN ('active','draining'))
  UNION
  SELECT p.environment_id,p.id,'revocation' FROM secrets s JOIN secret_exposures x ON (x.environment_id,x.secret_id)=(s.environment_id,s.id)
   JOIN computer_preparations p ON (p.environment_id,p.id)=(x.environment_id,x.preparation_id)
   WHERE s.status='revoked' AND p.status IN ('queued','running')
 ) SELECT environment_id,id,kind FROM candidates WHERE (environment_id,id,kind)>($1,$2,$3) ORDER BY environment_id,id,kind LIMIT 100`, after.environment, after.id, after.kind)
	if err != nil {
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (preparationLifecyclePosition, error) {
		var p preparationLifecyclePosition
		err := row.Scan(&p.environment, &p.id, &p.kind)
		return p, err
	})
	if err != nil {
		return after, false, err
	}
	if len(candidates) == 0 {
		return preparationLifecyclePosition{}, false, nil
	}
	var failures []error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		after = candidate
		var err error
		switch candidate.kind {
		case "waiter":
			err = expirePreparationWaiter(ctx, database, candidate.environment, candidate.id)
		case "attempt":
			err = expirePreparation(ctx, database, candidate.environment, candidate.id)
		case "unattached":
			err = reconcileUnattachedComputer(ctx, database, candidate.environment, candidate.id)
		case "image":
			err = reconcileComputerImage(ctx, database, candidate.environment, candidate.id)
		case "image_revocation":
			err = reconcileComputerImageRevocation(ctx, database, candidate.environment, candidate.id)
		case "revocation":
			err = reconcilePreparationRevocation(ctx, database, candidate.environment, candidate.id)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	more := len(candidates) == 100 || after != candidates[len(candidates)-1]
	if !more {
		after = preparationLifecyclePosition{}
	}
	return after, more, errors.Join(failures...)
}

// Attached waiters only resolve their existing chain; failure is never retried
// here. Never-attached demand is handled by reconcileUnattachedComputer.
func reconcileComputerImage(ctx context.Context, database db.TxDB, env, computer uuid.UUID) error {
	// Bound each waiter independently: contention must not spend another
	// Computer's scan/settlement budget. The scan context only owns discovery.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := PinComputerImage(ctx, database, env, computer); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotReady) {
		return err
	}
	var attached *uuid.UUID
	if err := database.QueryRow(ctx, `SELECT preparation_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&attached); err != nil {
		return err
	}
	if attached == nil {
		return nil
	}
	_, err := EnsurePreparationSuccessor(ctx, database, env, *attached)
	if errors.Is(err, ErrNotReady) {
		return nil
	}
	return err
}

// Unattached demand has never consumed an attempt. It may wait behind an
// unusable physical executor, but does not retry an attached failed chain.
func reconcileUnattachedComputer(ctx context.Context, database db.TxDB, env, computer uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := pinComputerImage(ctx, tx, env, computer); err == nil {
			return nil
		} else if !errors.Is(err, ErrNotReady) {
			return err
		}
		_, err := attachComputerPreparation(ctx, tx, env, computer)
		return err
	})
	if errors.Is(err, ErrNotReady) {
		return nil
	}
	return err
}
