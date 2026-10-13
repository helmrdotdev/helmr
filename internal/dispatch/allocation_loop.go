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
)

type AllocationPosition struct {
	environment uuid.UUID
	kind        string
	owner, host uuid.UUID
}

// No Host identity can sort after this cursor when the whole demand is blocked.
var lastAllocationHost = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

type allocationCandidate struct {
	AllocationPosition
	epoch int64
}

// Run owns placement discovery. Both demand and Host are in the scan cursor:
// a busy prefix of Hosts or unschedulable demand cannot hide later candidates.
// Each attempt is a separate transaction; only committed allocations survive a
// process restart, and retry always revalidates the discovered capacity.
func (a *Allocator) Run(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		return ErrInvalidInput
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var after AllocationPosition
	for {
		next, more, err := a.Reconcile(ctx, after)
		after = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "allocation reconciliation failed", "error", err)
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

func (a *Allocator) discoverAllocations(ctx context.Context, after AllocationPosition) ([]allocationCandidate, error) {
	scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := a.database.Query(scanCtx, `WITH demand AS (
 SELECT p.environment_id,p.id,'preparation'::text kind,1::bigint epoch,NULL::uuid host_id FROM image_builds p
 WHERE p.status='queued' AND p.worker_host_id IS NULL AND p.deadline_at>statement_timestamp()
 UNION ALL
 SELECT c.environment_id,c.id,'computer',COALESCE((SELECT max(l.epoch) FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id),0)+1,NULL::uuid FROM computers c
 WHERE c.initial_root_id IS NOT NULL AND c.image_id IS NOT NULL AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL AND c.recovery_save_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND (l.fenced_at IS NULL OR l.initialized_at IS NOT NULL))
 AND (EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('open','closing') AND `+session.NoSessionHoldsSQL+`) OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending'))
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.epoch=9223372036854775807)
 UNION ALL
 SELECT cp.environment_id,cp.id,'restore',COALESCE((SELECT max(l.epoch) FROM computer_leases l WHERE l.environment_id=cp.environment_id AND l.computer_id=cp.computer_id),0)+1,NULL::uuid
 FROM checkpoints cp WHERE cp.status IN ('ready','restoring')
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=cp.environment_id AND l.computer_id=cp.computer_id AND (l.fenced_at IS NULL OR l.epoch=9223372036854775807))
 AND (EXISTS(SELECT 1 FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
 WHERE s.environment_id=cp.environment_id AND s.computer_id=cp.computer_id AND s.status IN ('open','closing') AND t.status='queued' AND `+session.NoSessionHoldsSQL+`) OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=cp.environment_id AND cmd.computer_id=cp.computer_id AND cmd.status='pending'))
 AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked WHERE revoked.environment_id=cp.environment_id AND revoked.computer_id=cp.computer_id)
 UNION ALL
 SELECT s.environment_id,s.id,'process',COALESCE((SELECT max(p.epoch) FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id),0)+1,l.worker_host_id
 FROM sessions s JOIN computer_leases l ON (l.environment_id,l.computer_id)=(s.environment_id,s.computer_id)
 WHERE s.status IN ('open','closing') AND `+session.NoSessionHoldsSQL+` AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>statement_timestamp()
 AND EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued')
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND (p.fenced_at IS NULL OR p.epoch=9223372036854775807))
 ) SELECT d.environment_id,d.kind,d.id,h.id,d.epoch FROM demand d JOIN environments e ON e.id=d.environment_id AND e.retired_at IS NULL
 JOIN projects p ON p.id=e.project_id JOIN worker_groups g ON g.region_id=p.default_region_id AND g.status='active'
 JOIN worker_pools pool ON pool.worker_group_id=g.id AND (d.kind IN ('process','restore') OR pool.id=g.primary_pool_id) AND pool.status='active'
 JOIN worker_hosts h ON h.worker_group_id=g.id AND h.worker_pool_id=pool.id AND h.status='active' AND h.current_epoch IS NOT NULL
 AND h.observed_at>=statement_timestamp()-$5*interval '1 second'
 WHERE (d.host_id IS NULL OR h.id=d.host_id)
 AND (d.kind<>'restore' OR EXISTS(SELECT 1 FROM checkpoints cp
 JOIN computer_leases source ON (source.environment_id,source.computer_id,source.epoch)=(cp.environment_id,cp.computer_id,cp.source_lease_epoch)
 JOIN worker_hosts source_host ON source_host.id=source.worker_host_id
 WHERE cp.environment_id=d.environment_id AND cp.id=d.id AND source_host.worker_group_id=g.id))
 AND (d.environment_id,d.kind,d.id,h.id)>($1,$2,$3,$4)
 ORDER BY d.environment_id,d.kind,d.id,h.id LIMIT 100`, after.environment, after.kind, after.owner, after.host, workergroup.ObservationFreshnessSeconds)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (allocationCandidate, error) {
		var c allocationCandidate
		err := row.Scan(&c.environment, &c.kind, &c.owner, &c.host, &c.epoch)
		return c, err
	})
}

func (a *Allocator) Reconcile(ctx context.Context, after AllocationPosition) (AllocationPosition, bool, error) {
	candidates, err := a.discoverAllocations(ctx, after)
	if err != nil {
		return after, false, err
	}
	if len(candidates) == 0 {
		return AllocationPosition{}, false, nil
	}
	var failures []error
	for _, c := range candidates {
		if after.host == lastAllocationHost && after.environment == c.environment && after.kind == c.kind && after.owner == c.owner {
			continue
		}
		if ctx.Err() != nil {
			return after, false, ctx.Err()
		}
		attemptCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		if c.kind == "preparation" {
			_, err = a.AllocateImageBuild(attemptCtx, c.environment, c.owner, c.host)
		} else if c.kind == "restore" {
			_, err = a.AllocateRestoredLease(attemptCtx, c.environment, c.owner, c.epoch, c.host)
		} else if c.kind == "process" {
			_, err = a.AllocateSessionProcess(attemptCtx, c.environment, c.owner, c.epoch)
		} else {
			_, err = a.AllocateFreshLease(attemptCtx, c.environment, c.owner, c.epoch, c.host)
		}
		stop()
		after = c.AllocationPosition
		if errors.Is(err, errEnvironmentCapacity) {
			after.host = lastAllocationHost
		}
		if err != nil && !(errors.Is(err, ErrNotReady) || errors.Is(err, computer.ErrNotReady) || errors.Is(err, session.ErrNotReady)) && !(errors.Is(err, ErrDenied) || errors.Is(err, computer.ErrDenied) || errors.Is(err, session.ErrDenied)) && !errors.Is(err, context.DeadlineExceeded) {
			failures = append(failures, err)
		}
	}
	if len(candidates) < 100 {
		return AllocationPosition{}, false, errors.Join(failures...)
	}
	return after, true, errors.Join(failures...)
}
