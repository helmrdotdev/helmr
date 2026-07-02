package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/clickhouse"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), log); err != nil {
		log.Error("dispatcher stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadDispatcher()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	queries := db.New(pool)
	clickHouseClient, err := clickhouse.New(clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	telemetryReader := telemetry.NewCompositeReader(
		telemetry.NewHotReader(queries),
		telemetry.NewHistoricalReader(clickHouseClient),
	)

	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	queue, err := dispatchredis.New(redisClient)
	if err != nil {
		return fmt.Errorf("configure dispatch queue: %w", err)
	}
	enqueuer, err := dispatch.NewEnqueuer(queries, queue)
	if err != nil {
		return fmt.Errorf("configure dispatch enqueuer: %w", err)
	}
	eventStream, err := control.NewEventStream(log, queries, redisClient, control.EventStreamConfig{
		CellID:          cfg.CellID,
		TelemetryReader: telemetryReader,
	})
	if err != nil {
		return fmt.Errorf("configure event stream: %w", err)
	}
	telemetryIngestor, err := telemetry.NewIngestor(log, queries, telemetry.NewClickHouseWriter(clickHouseClient))
	if err != nil {
		return fmt.Errorf("configure telemetry ingester: %w", err)
	}
	scheduleIndex, err := schedule.NewRedisIndex(redisClient)
	if err != nil {
		return fmt.Errorf("configure schedule index: %w", err)
	}
	keyring, err := secret.KeyringFromBase64(cfg.SecretEncryptionKey, cfg.SecretEncryptionKeyOld)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	secretStore, err := secret.New(queries, keyring)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	sweeperLock, err := dispatch.NewExpirySweepAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure sweeper lock: %w", err)
	}
	sweeper, err := dispatch.NewExpirySweeper(
		queries,
		dispatch.WithExpirySweepLogger(log),
		dispatch.WithExpirySweepLock(sweeperLock),
	)
	if err != nil {
		return fmt.Errorf("configure sweeper: %w", err)
	}
	queueReconcileLock, err := dispatch.NewQueueReconcileAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure queue reconcile lock: %w", err)
	}
	queueReconciler, err := dispatch.NewQueueReconciler(
		queries,
		enqueuer,
		dispatch.WithQueueReconcileLogger(log),
		dispatch.WithQueueReconcileLock(queueReconcileLock),
	)
	if err != nil {
		return fmt.Errorf("configure queue reconciler: %w", err)
	}
	preparedRuntimeWarmLock, err := dispatch.NewRuntimePrepareAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure prepared runtime warm lock: %w", err)
	}
	preparedRuntimeWarmer, err := dispatch.NewRuntimePreparer(
		queries,
		dispatch.WithRuntimePrepareLogger(log),
		dispatch.WithRuntimePrepareLock(preparedRuntimeWarmLock),
		dispatch.WithRuntimePrepareTarget(int32(cfg.RuntimePrepareTarget)),
		dispatch.WithRuntimePrepareLimit(int32(cfg.RuntimePrepareLimit)),
		dispatch.WithRuntimePrepareInterval(cfg.RuntimePrepareEvery),
	)
	if err != nil {
		return fmt.Errorf("configure prepared runtime warmer: %w", err)
	}
	scheduleReconcileLock, err := schedule.NewReconcileAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure schedule reconcile lock: %w", err)
	}
	scheduleRunCreator, err := control.NewScheduleRunCreator(log, pool, secretStore, enqueuer, eventStream)
	if err != nil {
		return fmt.Errorf("configure schedule run creator: %w", err)
	}
	scheduleEngine, err := schedule.NewEngine(
		log,
		pool,
		scheduleIndex,
		scheduleRunCreator,
		schedule.EngineConfig{
			RepairLimit:     int32(cfg.ScheduleRepairLimit),
			RepairLookahead: cfg.ScheduleRepairLookahead,
			MaxAttempts:     int32(cfg.ScheduleMaxAttempts),
			Jitter:          cfg.ScheduleJitter,
			ReconcileLock:   scheduleReconcileLock,
		},
	)
	if err != nil {
		return fmt.Errorf("configure schedule engine: %w", err)
	}
	scheduleWorker, err := schedule.NewWorker(
		log,
		scheduleEngine,
		schedule.WithRepairEvery(cfg.ScheduleRepairEvery),
		schedule.WithRepairLimit(int32(cfg.ScheduleRepairLimit)),
		schedule.WithTriggerConcurrency(int32(cfg.ScheduleTriggerConcurrency)),
		schedule.WithLease(cfg.ScheduleLease),
	)
	if err != nil {
		return fmt.Errorf("configure schedule worker: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 5)
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		errc <- sweeper.Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		errc <- queueReconciler.Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		errc <- preparedRuntimeWarmer.Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		errc <- scheduleWorker.Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		errc <- telemetryIngestor.Run(runCtx)
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	log.Info("helmr dispatcher running")
	var firstErr error
	select {
	case <-ctx.Done():
		cancel()
	case err := <-errc:
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
		}
	}
	<-done
	close(errc)
	for err := range errc {
		if firstErr == nil && err != nil && !errors.Is(err, context.Canceled) {
			firstErr = err
		}
	}
	return firstErr
}
