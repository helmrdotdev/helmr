package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ExpireImageBuild makes deadline/authority loss visible without asserting that
// the physical writer stopped. A never-claimed attempt has no physical blocker.
func ExpireImageBuild(ctx context.Context, pool db.TxBeginner, env, id uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var host, group *uuid.UUID
		var spec uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT p.image_spec_id,p.worker_host_id,h.worker_group_id FROM image_builds p LEFT JOIN worker_hosts h ON h.id=p.worker_host_id WHERE p.environment_id=$1 AND p.id=$2`, env, id).Scan(&spec, &host, &group); err != nil {
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
		if err := computer.LockImageSpec(ctx, tx, env, spec); err != nil {
			return err
		}
		var same, deadline, unavailable bool
		var status string
		if err := tx.QueryRow(ctx, `SELECT p.worker_host_id IS NOT DISTINCT FROM $3::uuid,p.status,p.deadline_at<=clock_timestamp(),
    COALESCE(p.executor_expires_at<=clock_timestamp() OR h.current_epoch IS DISTINCT FROM p.worker_epoch OR h.status NOT IN ('active','draining'),false)
   FROM image_builds p LEFT JOIN worker_hosts h ON h.id=p.worker_host_id WHERE p.environment_id=$1 AND p.id=$2 FOR NO KEY UPDATE OF p`, env, id, host).Scan(&same, &status, &deadline, &unavailable); err != nil {
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
		return computer.TerminateImageBuild(ctx, tx, env, id, code)
	}))
}

// ReconcileImageBuildRevocation closes logical execution permission. It does
// not release the unfenced-writer index or claim that the host has stopped.
func ReconcileImageBuildRevocation(ctx context.Context, pool db.TxBeginner, env, id uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		p, err := computer.ReadImageBuild(ctx, tx, env, id)
		if err != nil {
			return err
		}
		// Lock exposures' Secrets before the spec. The attempt's exposure set can
		// only grow under these same Secrets and spec locks, then is immutable.
		rows, err := tx.Query(ctx, `SELECT s.id FROM secrets s JOIN secret_exposures x ON (x.environment_id,x.secret_id)=(s.environment_id,s.id)
   WHERE x.environment_id=$1 AND x.image_build_id=$2 ORDER BY s.id FOR SHARE OF s`, env, id)
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
		if err = computer.LockImageSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		return computer.FailRevokedImageBuild(ctx, tx, env, id)
	}))
}

type stoppedImageBuild struct {
	env, spec, id, instance uuid.UUID
	hostEpoch               int64
	seed                    bool
}

// Private build qualification VMs have fresh physical identities. If recovery
// retains any unknown VM, it cannot prove all children of a private build absent.
// Provider-confirmed host absence supplies an empty quarantine set.
func hostImageBuilds(ctx context.Context, tx pgx.Tx, host uuid.UUID, beforeEpoch *int64, quarantined []uuid.UUID) ([]stoppedImageBuild, error) {
	rows, err := tx.Query(ctx, `SELECT environment_id,image_spec_id,id,allocation_id,worker_epoch,false AS seed FROM image_builds
  WHERE worker_host_id=$1 AND fenced_at IS NULL AND ($2::bigint IS NULL OR worker_epoch<$2) AND cardinality($3::uuid[])=0
  UNION ALL
  SELECT environment_id,image_spec_id,id,allocation_id,worker_epoch,true FROM boot_seed_builds
  WHERE worker_host_id=$1 AND fenced_at IS NULL AND ($2::bigint IS NULL OR worker_epoch<$2) AND cardinality($3::uuid[])=0
  ORDER BY environment_id,image_spec_id,id`, host, beforeEpoch, quarantined)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (stoppedImageBuild, error) {
		var p stoppedImageBuild
		err := row.Scan(&p.env, &p.spec, &p.id, &p.instance, &p.hostEpoch, &p.seed)
		return p, err
	})
}

// Called with provider-confirmation supply locks before any Session/Computer
// lock, so all specs precede the full Computer set in the shared transaction.
func lockAbsentHostImageBuilds(ctx context.Context, tx pgx.Tx, host uuid.UUID) ([]stoppedImageBuild, error) {
	attempts, err := hostImageBuilds(ctx, tx, host, nil, []uuid.UUID{})
	if err != nil {
		return nil, err
	}
	for _, p := range attempts {
		if err = computer.LockImageSpec(ctx, tx, p.env, p.spec); err != nil {
			return nil, err
		}
	}
	return attempts, nil
}
func observeRecoveredHostImageBuilds(ctx context.Context, database db.TxDB, host workergroup.HostPrincipal, quarantined []uuid.UUID) error {
	var attempts []stoppedImageBuild
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockRecoveringComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var err error
		attempts, err = hostImageBuilds(ctx, tx, host.HostID, &host.Epoch, quarantined)
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
			if err := computer.LockImageSpec(ctx, tx, p.env, p.spec); err != nil {
				return err
			}
			return recordBuildStopped(ctx, tx, p, "successor worker completed local VM inventory and physical cleanup")
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

func recordBuildStopped(ctx context.Context, tx pgx.Tx, build stoppedImageBuild, evidence string) error {
	if build.seed {
		return computer.RecordBootSeedStopped(ctx, tx, build.env, build.id, evidence)
	}
	return computer.RecordImageBuildStopped(ctx, tx, build.env, build.id, evidence)
}

type ImageBuildLifecyclePosition struct {
	environment, id uuid.UUID
	kind            string
}

// RunImageBuildLifecycle owns due waiter settlement, attempt expiry and revoked
// exposure reconciliation and periodic image refresh independently of guest connectivity.
func RunImageBuildLifecycle(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("preparation lifecycle database and logger are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var after ImageBuildLifecyclePosition
	for {
		next, more, err := ReconcileImageBuildLifecycle(ctx, database, after)
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
func ReconcileImageBuildLifecycle(ctx context.Context, database db.TxDB, after ImageBuildLifecyclePosition) (ImageBuildLifecyclePosition, bool, error) {
	scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := database.Query(scanCtx, `WITH candidates AS (
  SELECT environment_id,id,'waiter'::text AS kind FROM computers WHERE initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at<=statement_timestamp()
  UNION
  SELECT c.environment_id,c.id,'unattached' FROM computers c JOIN environments e ON e.id=c.environment_id WHERE c.initial_root_digest IS NULL AND image_build_id IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at>statement_timestamp()
  UNION
  SELECT c.environment_id,c.id,'image' FROM computers c JOIN image_builds p ON (p.environment_id,p.id)=(c.environment_id,c.image_build_id)
   WHERE c.initial_root_digest IS NULL AND p.status='succeeded' AND c.preparation_failed_at IS NULL AND c.deleted_at IS NULL AND c.preparation_deadline_at>statement_timestamp()
  UNION
  SELECT revoked.environment_id,revoked.computer_id,'secret_revocation' FROM (`+db.RevokedComputerSecretsSQL+`) revoked
   JOIN session_processes p ON (p.environment_id,p.computer_id)=(revoked.environment_id,revoked.computer_id)
   WHERE p.fenced_at IS NULL AND (p.status IN ('starting','ready') OR EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=p.environment_id AND t.session_id=p.session_id AND t.process_epoch=p.epoch AND t.status='running'))
  UNION
  SELECT s.environment_id,s.id,'refresh' FROM image_specs s JOIN environments e ON e.id=s.environment_id
   WHERE s.next_refresh_at<=statement_timestamp()
   AND NOT EXISTS(SELECT 1 FROM secret_bindings b JOIN secrets x ON (x.environment_id,x.id)=(b.environment_id,b.secret_id)
     WHERE b.environment_id=s.environment_id AND b.image_spec_id=s.id AND x.status='revoked')
   AND NOT EXISTS(SELECT 1 FROM image_builds p WHERE p.environment_id=s.environment_id AND p.image_spec_id=s.id
     AND p.status NOT IN ('queued','running') AND p.worker_host_id IS NOT NULL AND p.fenced_at IS NULL)
  UNION
  SELECT environment_id,id,'attempt' FROM image_builds WHERE status IN ('queued','running') AND deadline_at<=statement_timestamp()
  UNION
  SELECT b.environment_id,b.id,'seed' FROM boot_seed_builds b LEFT JOIN worker_hosts h ON h.id=b.worker_host_id
   WHERE b.status='running' AND (b.deadline_at<=statement_timestamp() OR b.executor_expires_at<=statement_timestamp()
     OR h.current_epoch IS DISTINCT FROM b.worker_epoch OR h.status NOT IN ('active','draining'))
  UNION
  SELECT environment_id,id,'attempt' FROM image_builds WHERE status='running' AND executor_expires_at<=statement_timestamp()
  UNION
  SELECT p.environment_id,p.id,'attempt' FROM worker_hosts h JOIN image_builds p ON p.worker_host_id=h.id
   WHERE p.status='running' AND (h.current_epoch IS DISTINCT FROM p.worker_epoch OR h.status NOT IN ('active','draining'))
  UNION
  SELECT p.environment_id,p.id,'revocation' FROM secrets s JOIN secret_exposures x ON (x.environment_id,x.secret_id)=(s.environment_id,s.id)
   JOIN image_builds p ON (p.environment_id,p.id)=(x.environment_id,x.image_build_id)
   WHERE s.status='revoked' AND p.status IN ('queued','running')
 ) SELECT environment_id,id,kind FROM candidates WHERE (environment_id,id,kind)>($1,$2,$3) ORDER BY environment_id,id,kind LIMIT 100`, after.environment, after.id, after.kind)
	if err != nil {
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ImageBuildLifecyclePosition, error) {
		var p ImageBuildLifecyclePosition
		err := row.Scan(&p.environment, &p.id, &p.kind)
		return p, err
	})
	if err != nil {
		return after, false, err
	}
	if len(candidates) == 0 {
		return ImageBuildLifecyclePosition{}, false, nil
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
			err = session.ExpireImageWaiter(ctx, database, candidate.environment, candidate.id)
		case "attempt":
			err = ExpireImageBuild(ctx, database, candidate.environment, candidate.id)
		case "seed":
			err = expireBootSeed(ctx, database, candidate.environment, candidate.id)
		case "unattached":
			err = reconcileUnattachedComputer(ctx, database, candidate.environment, candidate.id)
		case "image":
			err = reconcileComputerImage(ctx, database, candidate.environment, candidate.id)
		case "secret_revocation":
			err = session.ReconcileComputerSecretRevocation(ctx, database, candidate.environment, candidate.id)
		case "revocation":
			err = ReconcileImageBuildRevocation(ctx, database, candidate.environment, candidate.id)
		case "refresh":
			err = reconcileImageRefresh(ctx, database, candidate.environment, candidate.id)
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	more := len(candidates) == 100 || after != candidates[len(candidates)-1]
	if !more {
		after = ImageBuildLifecyclePosition{}
	}
	return after, more, errors.Join(failures...)
}

// Attached waiters only resolve their existing chain; failure is never retried
// here. Never-attached demand is handled by reconcileUnattachedComputer.
func reconcileComputerImage(ctx context.Context, database db.TxDB, env, computerID uuid.UUID) error {
	// Bound each waiter independently: contention must not spend another
	// Computer's scan/settlement budget. The scan context only owns discovery.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := computer.PinImage(ctx, database, env, computerID); err == nil {
		return nil
	} else if !errors.Is(err, computer.ErrNotReady) {
		return err
	}
	var attached *uuid.UUID
	if err := database.QueryRow(ctx, `SELECT image_build_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computerID).Scan(&attached); err != nil {
		return err
	}
	if attached == nil {
		return nil
	}
	_, err := computer.EnsureImageBuildSuccessor(ctx, database, env, *attached)
	if errors.Is(err, computer.ErrNotReady) {
		return nil
	}
	return err
}

// Unattached demand has never consumed an attempt. It may wait behind an
// unusable physical executor, but does not retry an attached failed chain.
func reconcileUnattachedComputer(ctx context.Context, database db.TxDB, env, computerID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := computer.PinAdmissionImage(ctx, tx, env, computerID); err == nil {
			return nil
		} else if !errors.Is(err, computer.ErrNotReady) {
			return err
		}
		_, err := computer.AttachImageBuild(ctx, tx, env, computerID)
		return err
	})
	if errors.Is(err, computer.ErrNotReady) {
		return nil
	}
	return err
}

func reconcileImageRefresh(ctx context.Context, database db.TxDB, env, spec uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := computer.RefreshImageSpec(ctx, database, env, spec)
	if errors.Is(err, computer.ErrNotReady) {
		return nil
	}
	return err
}
