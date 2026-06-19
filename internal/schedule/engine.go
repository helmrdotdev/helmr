package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Instance struct {
	InstanceID pgtype.UUID
	Generation int64
	Active     bool
	NextFireAt pgtype.Timestamptz
	RetryAfter pgtype.Timestamptz
}

type AdvanceInstance struct {
	InstanceID       pgtype.UUID
	Generation       int64
	CurrentFireAt    pgtype.Timestamptz
	NextFireAt       pgtype.Timestamptz
	LastTriggerRunID pgtype.UUID
}

type EngineConfig struct {
	RepairLimit     int32
	RepairLookahead time.Duration
	MaxAttempts     int32
	Jitter          time.Duration
	ReconcileLock   ReconcileLock
	Now             func() time.Time
}

type Engine struct {
	log         *slog.Logger
	db          *db.Queries
	lock        ReconcileLock
	index       Index
	runner      RunCreator
	repairLimit int32
	lookahead   time.Duration
	maxAttempts int32
	jitter      time.Duration
	now         func() time.Time
}

func NewEngine(log *slog.Logger, database dbConn, index Index, runner RunCreator, cfg EngineConfig) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}
	if index == nil {
		return nil, errors.New("schedule index is required")
	}
	repairLimit := cfg.RepairLimit
	if repairLimit <= 0 {
		repairLimit = DefaultRepairLimit
	}
	lookahead := cfg.RepairLookahead
	if lookahead <= 0 {
		lookahead = DefaultRepairLookahead
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	jitter := cfg.Jitter
	if jitter <= 0 {
		jitter = DefaultJitter
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	engine := &Engine{
		log:         log,
		lock:        cfg.ReconcileLock,
		index:       index,
		runner:      runner,
		repairLimit: repairLimit,
		lookahead:   lookahead,
		maxAttempts: maxAttempts,
		jitter:      jitter,
		now:         now,
	}
	if database != nil {
		engine.db = db.New(database)
	}
	return engine, nil
}

func (e *Engine) RegisterNext(ctx context.Context, instance Instance) error {
	if !instance.Active || !instance.NextFireAt.Valid {
		return nil
	}
	instanceID, err := pgvalue.UUIDValue(instance.InstanceID)
	if err != nil {
		return fmt.Errorf("schedule instance id is invalid: %v", err)
	}
	nextFireAt := instance.NextFireAt.Time.UTC()
	availableAt, err := e.availableAt(instance.InstanceID, instance.NextFireAt, instance.RetryAfter)
	if err != nil {
		return err
	}
	return e.index.Enqueue(ctx, IndexEntry{
		InstanceID:  instanceID,
		Generation:  instance.Generation,
		ScheduledAt: nextFireAt,
		AvailableAt: availableAt,
	})
}

func (e *Engine) DeleteInstance(ctx context.Context, instanceID pgtype.UUID) error {
	value, err := pgvalue.UUIDValue(instanceID)
	if err != nil {
		return fmt.Errorf("schedule instance id is invalid: %v", err)
	}
	return e.index.Delete(ctx, value)
}

func (e *Engine) Fire(ctx context.Context, lease IndexLease) error {
	if e.db == nil {
		return errors.New("schedule database is required")
	}
	if e.runner == nil {
		return errors.New("schedule run creator is required")
	}
	candidate, err := e.db.GetScheduleTriggerCandidate(ctx, db.GetScheduleTriggerCandidateParams{
		InstanceID:  pgvalue.UUID(lease.Entry.InstanceID),
		Generation:  lease.Entry.Generation,
		ScheduledAt: pgvalue.TimestamptzUTCZeroInvalid(lease.Entry.ScheduledAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		retryAfter, retryErr := e.db.GetScheduleRetryAfter(ctx, db.GetScheduleRetryAfterParams{
			InstanceID:  pgvalue.UUID(lease.Entry.InstanceID),
			Generation:  lease.Entry.Generation,
			ScheduledAt: pgvalue.TimestamptzUTCZeroInvalid(lease.Entry.ScheduledAt),
		})
		if retryErr == nil && retryAfter.Valid {
			return e.index.Nack(ctx, lease, retryAfter.Time.UTC())
		}
		if retryErr != nil && !errors.Is(retryErr, pgx.ErrNoRows) {
			return retryErr
		}
		return e.index.Ack(ctx, lease)
	}
	if err != nil {
		return err
	}
	next, err := e.nextFireAt(candidate.Cron, candidate.Timezone, candidate.NextFireAt, e.now())
	if err != nil {
		return e.markTriggerFailed(ctx, lease, candidate, err)
	}
	runID, err := e.runner.CreateScheduleRun(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrTriggerSuperseded) {
			return e.index.Ack(ctx, lease)
		}
		if errors.Is(err, ErrTriggerDeferred) {
			return e.deferTrigger(ctx, lease, candidate)
		}
		return e.markTriggerFailed(ctx, lease, candidate, err)
	}
	if !runID.Valid {
		return errors.New("created schedule run has no id")
	}
	advanced, err := e.Advance(ctx, AdvanceInstance{
		InstanceID:       candidate.InstanceID,
		Generation:       candidate.Generation,
		CurrentFireAt:    candidate.NextFireAt,
		NextFireAt:       pgvalue.TimestamptzUTCZeroInvalid(next),
		LastTriggerRunID: runID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return e.index.Ack(ctx, lease)
	} else if err != nil {
		return err
	}
	if err := e.index.Ack(ctx, lease); err != nil {
		return err
	}
	return e.RegisterNext(ctx, advanced)
}

func (e *Engine) Advance(ctx context.Context, instance AdvanceInstance) (Instance, error) {
	advanced, err := e.db.AdvanceScheduleInstance(ctx, db.AdvanceScheduleInstanceParams{
		NextFireAt:       instance.NextFireAt,
		LastFireAt:       instance.CurrentFireAt,
		LastTriggerRunID: instance.LastTriggerRunID,
		InstanceID:       instance.InstanceID,
		Generation:       instance.Generation,
	})
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		InstanceID: advanced.InstanceID,
		Generation: advanced.Generation,
		Active:     true,
		NextFireAt: advanced.NextFireAt,
	}, nil
}

func (e *Engine) Repair(ctx context.Context) error {
	if e.db == nil {
		return errors.New("schedule database is required")
	}
	store := RepairStore(e.db)
	var guard ReconcileLockGuard
	if e.lock != nil {
		var locked bool
		var err error
		guard, locked, err = e.lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !locked {
			e.log.Debug("schedule repair lock is held by another instance")
			return nil
		}
		store = guard.Store(store)
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileUnlockTimeout)
			defer cancel()
			if err := guard.Unlock(unlockCtx); err != nil {
				e.log.Warn("release schedule repair lock failed", "error", err)
			}
		}()
	}
	availableBefore := pgvalue.TimestamptzUTCZeroInvalid(e.now().Add(e.lookahead))
	var afterAvailableAt pgtype.Timestamptz
	var afterInstanceID pgtype.UUID
	for {
		rows, err := store.ListScheduleRepairEntries(ctx, db.ListScheduleRepairEntriesParams{
			AvailableBefore:  availableBefore,
			AfterAvailableAt: afterAvailableAt,
			AfterInstanceID:  afterInstanceID,
			RowLimit:         e.repairLimit,
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := e.RegisterNext(ctx, Instance{
				InstanceID: row.InstanceID,
				Generation: row.Generation,
				Active:     true,
				NextFireAt: row.NextFireAt,
				RetryAfter: row.RetryAfter,
			}); err != nil {
				return err
			}
		}
		if len(rows) < int(e.repairLimit) {
			break
		}
		last := rows[len(rows)-1]
		afterAvailableAt = last.AvailableAt
		afterInstanceID = last.InstanceID
	}
	return nil
}

func (e *Engine) markTriggerFailed(ctx context.Context, lease IndexLease, row db.GetScheduleTriggerCandidateRow, cause error) error {
	nextAttempt := row.TriggerAttemptCount + 1
	retryAt := e.now().Add(RetryDelay(nextAttempt))
	_, err := e.db.MarkScheduleInstanceTriggerFailed(ctx, db.MarkScheduleInstanceTriggerFailedParams{
		ErrorKind:    triggerErrorKind(cause),
		ErrorMessage: cause.Error(),
		RetryAfter:   pgvalue.TimestamptzUTCZeroInvalid(retryAt),
		InstanceID:   row.InstanceID,
		Generation:   row.Generation,
		ScheduledAt:  row.NextFireAt,
	})
	if err != nil {
		return err
	}
	if nextAttempt >= e.maxAttempts {
		return e.skipFailedFire(ctx, lease, row)
	}
	if nackErr := e.index.Nack(ctx, lease, retryAt); nackErr != nil {
		return nackErr
	}
	return cause
}

func (e *Engine) deferTrigger(ctx context.Context, lease IndexLease, row db.GetScheduleTriggerCandidateRow) error {
	indexAttempt := lease.Attempt
	if indexAttempt < 1 {
		indexAttempt = 1
	}
	retryAt := e.now().Add(RetryDelay(row.TriggerAttemptCount + indexAttempt))
	affected, err := e.db.DeferScheduleInstanceTrigger(ctx, db.DeferScheduleInstanceTriggerParams{
		RetryAfter:  pgvalue.TimestamptzUTCZeroInvalid(retryAt),
		InstanceID:  row.InstanceID,
		Generation:  row.Generation,
		ScheduledAt: row.NextFireAt,
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return e.index.Ack(ctx, lease)
	}
	return e.index.Nack(ctx, lease, retryAt)
}

func (e *Engine) skipFailedFire(ctx context.Context, lease IndexLease, row db.GetScheduleTriggerCandidateRow) error {
	next, err := e.nextFireAt(row.Cron, row.Timezone, row.NextFireAt, e.now())
	if err != nil {
		affected, stopErr := e.db.StopScheduleInstanceTrigger(ctx, db.StopScheduleInstanceTriggerParams{
			InstanceID:  row.InstanceID,
			Generation:  row.Generation,
			ScheduledAt: row.NextFireAt,
		})
		if stopErr != nil {
			return stopErr
		}
		if affected == 0 {
			return e.index.Ack(ctx, lease)
		}
		if ackErr := e.index.Ack(ctx, lease); ackErr != nil {
			return ackErr
		}
		return err
	}
	skipped, err := e.db.SkipScheduleInstanceTrigger(ctx, db.SkipScheduleInstanceTriggerParams{
		NextFireAt: pgvalue.TimestamptzUTCZeroInvalid(next),
		InstanceID: row.InstanceID,
		Generation: row.Generation,
		LastFireAt: row.NextFireAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return e.index.Ack(ctx, lease)
	}
	if err != nil {
		return err
	}
	if err := e.index.Ack(ctx, lease); err != nil {
		return err
	}
	return e.RegisterNext(ctx, Instance{
		InstanceID: skipped.InstanceID,
		Generation: skipped.Generation,
		Active:     true,
		NextFireAt: skipped.NextFireAt,
	})
}

func (e *Engine) nextFireAt(cronExpr string, timezone string, nextFireAt pgtype.Timestamptz, now time.Time) (time.Time, error) {
	anchor := nextFireAt.Time.UTC()
	if anchor.Before(now) {
		anchor = now
	}
	return NextCronTime(cronExpr, timezone, anchor)
}

func (e *Engine) availableAt(instanceID pgtype.UUID, scheduledAt pgtype.Timestamptz, retryAfter pgtype.Timestamptz) (time.Time, error) {
	if retryAfter.Valid {
		return retryAfter.Time.UTC(), nil
	}
	id, err := pgvalue.UUIDValue(instanceID)
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule instance id is invalid: %v", err)
	}
	return scheduledAt.Time.UTC().Add(Jitter(id, e.jitter)), nil
}
