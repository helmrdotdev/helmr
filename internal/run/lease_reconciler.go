package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	leaseRecoveryLockName        = "helmr.dispatcher.run_resume_recovery"
	defaultLeaseRecoveryInterval = 5 * time.Second
	defaultLeaseRecoveryTimeout  = 15 * time.Second
	defaultLeaseRecoveryLimit    = int32(50)
)

// LeaseReconciler recovers lost Run execution leases in bounded cycles. A
// session-level advisory lock keeps one reconciler active across dispatchers;
// each candidate is still relocked in its own transaction.
type LeaseReconciler struct {
	tryLock       func(context.Context) (leaseRecoveryGuard, bool, error)
	recoverLeases func(context.Context, int32) (int, error)
	interval      time.Duration
	timeout       time.Duration
	limit         int32
	log           *slog.Logger
}

type leaseRecoveryGuard interface {
	Unlock() error
}

// NewLeaseReconciler holds its singleton lock on one pooled connection and
// runs discovery and each recovery transaction on other connections from the
// same pool.
func NewLeaseReconciler(pool *pgxpool.Pool, log *slog.Logger) (*LeaseReconciler, error) {
	if pool == nil {
		return nil, errors.New("Run lease recovery pool is required")
	}
	if log == nil {
		log = slog.Default()
	}
	key := pglock.Key(leaseRecoveryLockName)
	return &LeaseReconciler{
		tryLock: func(ctx context.Context) (leaseRecoveryGuard, bool, error) {
			guard, locked, err := pglock.TryAcquire(ctx, pool, key)
			if err != nil || !locked {
				return nil, locked, err
			}
			return guard, true, nil
		},
		recoverLeases: func(ctx context.Context, limit int32) (int, error) {
			return RecoverExecutionLeases(ctx, pool, limit)
		},
		interval: defaultLeaseRecoveryInterval,
		timeout:  defaultLeaseRecoveryTimeout,
		limit:    defaultLeaseRecoveryLimit,
		log:      log,
	}, nil
}

func (r *LeaseReconciler) Run(ctx context.Context) error {
	for {
		cycle, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.reconcile(cycle)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.interval
		if err != nil {
			r.log.Warn("Run lease recovery failed", "error", err)
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *LeaseReconciler) reconcile(ctx context.Context) error {
	guard, locked, err := r.tryLock(ctx)
	if err != nil || !locked {
		return err
	}
	defer func() {
		if err := guard.Unlock(); err != nil {
			r.log.Warn("release Run lease recovery lock failed", "error", err)
		}
	}()
	_, err = r.recoverLeases(ctx, r.limit)
	return err
}

// RecoverExecutionLeases settles lost logical grants, including grants from a
// committed restore. It cannot make the consumed checkpoint reusable.
// Discovery runs directly on database; each candidate is recovered in its own
// transaction.
func RecoverExecutionLeases(ctx context.Context, database db.TxDB, limit int32) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	candidates, err := db.New(database).ListRunExecutionLeaseRecoveryCandidates(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list Run execution lease recovery candidates: %w", err)
	}
	recovered := 0
	var recoveryErrors []error
	for _, candidate := range candidates {
		changed, err := recoverExecutionLease(ctx, database, candidate)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			recoveryErrors = append(recoveryErrors, err)
			continue
		}
		if changed {
			recovered++
		}
	}
	return recovered, errors.Join(recoveryErrors...)
}

func recoverExecutionLease(
	ctx context.Context,
	database db.TxBeginner,
	candidate db.ListRunExecutionLeaseRecoveryCandidatesRow,
) (bool, error) {
	runID, err := leaseRecoveryUUID(candidate.RunID)
	if err != nil {
		return false, err
	}
	computerID, err := leaseRecoveryUUID(candidate.ComputerID)
	if err != nil {
		return false, err
	}
	runLeaseID, err := leaseRecoveryUUID(candidate.RunLeaseID)
	if err != nil {
		return false, err
	}
	orgID, err := leaseRecoveryUUID(candidate.OrgID)
	if err != nil {
		return false, err
	}
	projectID, err := leaseRecoveryUUID(candidate.ProjectID)
	if err != nil {
		return false, err
	}
	environmentID, err := leaseRecoveryUUID(candidate.EnvironmentID)
	if err != nil {
		return false, err
	}
	tx, err := database.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin Run execution lease recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	graph, err := LockExecutionLeaseRecovery(ctx, tx, OwnedFinalizationRequest{
		OrgID: orgID, ProjectID: projectID, EnvironmentID: environmentID, RunID: runID,
	})
	if err != nil {
		return false, fmt.Errorf("lock Run execution lease recovery graph: %w", err)
	}
	recovered, err := graph.RecoverExecutionLeaseLoss(ctx, ExecutionLeaseRecoveryRequest{
		RunID: runID, ComputerID: computerID, AttemptNumber: candidate.CurrentAttemptNumber,
		RunLeaseID: runLeaseID,
	})
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit fresh Run lease recovery: %w", err)
	}
	return recovered, nil
}

func leaseRecoveryUUID(value pgtype.UUID) (uuid.UUID, error) {
	if !value.Valid {
		return uuid.Nil(), errors.New("Run lease recovery UUID is required")
	}
	parsed := uuid.UUID(value.Bytes)
	if parsed == uuid.Nil() {
		return uuid.Nil(), errors.New("Run lease recovery UUID is required")
	}
	return parsed, nil
}
