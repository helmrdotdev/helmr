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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	baseMaxConns          = int32(12)
	runDispatchMaxConns   = int32(4)
	buildDispatchMaxConns = int32(2)
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := runDispatcher(context.Background(), log); err != nil {
		log.Error("dispatcher stopped", "error", err)
		os.Exit(1)
	}
}

func runDispatcher(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadDispatcher()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := newDispatchPool(ctx, cfg.DatabaseURL, baseMaxConns)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	runDispatchPool, err := newDispatchPool(ctx, cfg.DatabaseURL, runDispatchMaxConns)
	if err != nil {
		return fmt.Errorf("configure run dispatch database pool: %w", err)
	}
	defer runDispatchPool.Close()
	buildDispatchPool, err := newDispatchPool(ctx, cfg.DatabaseURL, buildDispatchMaxConns)
	if err != nil {
		return fmt.Errorf("configure build dispatch database pool: %w", err)
	}
	defer buildDispatchPool.Close()
	connectionBudget := baseMaxConns + runDispatchMaxConns + buildDispatchMaxConns
	log.Info("dispatcher database connection budget", "max_connections", connectionBudget,
		"base", baseMaxConns, "run_dispatch", runDispatchMaxConns, "build_dispatch", buildDispatchMaxConns)
	queries := db.New(pool)
	runDispatchQueries := db.New(runDispatchPool)
	buildDispatchQueries := db.New(buildDispatchPool)
	workspaceFencingKey, err := workspace.NewFencingKey(cfg.WorkspaceFencingKey)
	if err != nil {
		return fmt.Errorf("configure workspace fencing key: %w", err)
	}
	runDispatchAuthority, err := dispatch.NewRunAuthority(
		runDispatchPool,
		workspaceFencingKey,
	)
	if err != nil {
		return fmt.Errorf("configure run dispatch authority: %w", err)
	}
	buildDispatchAuthority, err := dispatch.NewBuildAuthority(buildDispatchPool)
	if err != nil {
		return fmt.Errorf("configure build dispatch authority: %w", err)
	}
	clickHouseClient, err := clickhouse.New(clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
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
	wakePublisher, err := dispatchredis.NewWakePublisher(redisClient)
	if err != nil {
		return fmt.Errorf("configure worker wake publisher: %w", err)
	}
	placementReconciler, err := dispatch.NewPlacementReconciler(
		runDispatchQueries, runDispatchAuthority,
		buildDispatchQueries, buildDispatchAuthority,
		runDispatchQueries, runDispatchAuthority,
		queue, wakePublisher, log,
	)
	if err != nil {
		return fmt.Errorf("configure placement reconciler: %w", err)
	}
	runEnqueuer, err := dispatch.NewEnqueuer(runDispatchQueries, queue)
	if err != nil {
		return fmt.Errorf("configure run dispatch enqueuer: %w", err)
	}
	buildEnqueuer, err := dispatch.NewEnqueuer(buildDispatchQueries, queue)
	if err != nil {
		return fmt.Errorf("configure build dispatch enqueuer: %w", err)
	}
	telemetryIngestor, err := telemetry.NewIngestor(log, queries, clickhouse.NewWriter(clickHouseClient))
	if err != nil {
		return fmt.Errorf("configure telemetry ingester: %w", err)
	}
	buildSweeperLock, err := dispatch.NewBuildExpirySweepAdvisoryLock(buildDispatchPool)
	if err != nil {
		return fmt.Errorf("configure build sweeper lock: %w", err)
	}
	buildSweeper, err := dispatch.NewBuildExpirySweeper(
		buildDispatchQueries,
		dispatch.WithBuildExpirySweepLogger(log),
		dispatch.WithBuildExpirySweepLock(buildSweeperLock),
	)
	if err != nil {
		return fmt.Errorf("configure build sweeper: %w", err)
	}
	staleWorkerTransactions, err := dispatch.NewPGXStaleWorkerFenceTransactions(pool)
	if err != nil {
		return fmt.Errorf("configure stale worker fence transactions: %w", err)
	}
	staleWorkerLock, err := dispatch.NewStaleWorkerFenceAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure stale worker fence lock: %w", err)
	}
	staleWorkerFencer, err := dispatch.NewStaleWorkerFencer(staleWorkerTransactions,
		dispatch.WithStaleWorkerFenceLock(staleWorkerLock),
		dispatch.WithStaleWorkerFenceLogger(log),
	)
	if err != nil {
		return fmt.Errorf("configure stale worker fencer: %w", err)
	}
	queueReconcileLock, err := dispatch.NewQueueReconcileAdvisoryLock(runDispatchPool)
	if err != nil {
		return fmt.Errorf("configure queue reconcile lock: %w", err)
	}
	buildQueueReconcileLock, err := dispatch.NewBuildQueueReconcileAdvisoryLock(buildDispatchPool)
	if err != nil {
		return fmt.Errorf("configure build queue reconcile lock: %w", err)
	}
	queueReconciler, err := dispatch.NewQueueReconciler(
		runDispatchQueries,
		runEnqueuer,
		buildEnqueuer,
		dispatch.WithQueueReconcileLogger(log),
		dispatch.WithQueueReconcileLock(queueReconcileLock),
		dispatch.WithBuildQueueReconcileLock(buildQueueReconcileLock),
		dispatch.WithRunResumeRecoverer(runDispatchAuthority),
	)
	if err != nil {
		return fmt.Errorf("configure queue reconciler: %w", err)
	}
	scheduleAuthority := deployment.NewScheduleAuthority()
	scheduleAdmitter, err := schedule.NewDBAdmitter(pool, scheduleAuthority)
	if err != nil {
		return fmt.Errorf("configure schedule admission: %w", err)
	}
	scheduleWorker, err := schedule.NewWorker(log, queries, scheduleAdmitter)
	if err != nil {
		return fmt.Errorf("configure schedule worker: %w", err)
	}
	runAdmissionDelivery, err := run.NewDeliveryWorker(
		log,
		queries,
		func(ctx context.Context, orgID, runID pgtype.UUID) error {
			_, err := runEnqueuer.EnqueueRun(ctx, orgID, runID)
			if errors.Is(err, dispatch.ErrNoEnqueueCandidate) {
				return nil
			}
			return err
		},
		func(ctx context.Context, environmentID, runID, runWaitID pgtype.UUID, resumeRequestVersion int64) error {
			_, err := runEnqueuer.EnqueueRunResume(ctx, environmentID, runID, runWaitID, resumeRequestVersion)
			if errors.Is(err, dispatch.ErrStaleEnqueueHint) {
				return nil
			}
			return err
		},
	)
	if err != nil {
		return fmt.Errorf("configure run admission delivery: %w", err)
	}
	tokenWaitReconciler, err := token.NewWaitReconciler(pool)
	if err != nil {
		return fmt.Errorf("configure token wait reconciler: %w", err)
	}
	tokenReconcileDelivery, err := token.NewDeliveryWorker(
		log,
		queries,
		tokenWaitReconciler.ReconcileBatch,
	)
	if err != nil {
		return fmt.Errorf("configure token reconciliation delivery: %w", err)
	}
	secretRevocationReconciler, err := secret.NewRevocationReconciler(
		runDispatchPool,
		secret.WorkspaceExecRecoverer(func(
			ctx context.Context,
			candidate secret.WorkspaceExecCandidate,
		) error {
			err := runDispatchAuthority.RecoverWorkspaceExec(
				ctx,
				dispatch.RecoverableWorkspaceExecCandidate{
					OrgID:                candidate.OrgID,
					ProcessID:            candidate.ProcessID,
					WorkspaceID:          candidate.WorkspaceID,
					ExpectedStateVersion: candidate.ExpectedStateVersion,
				},
			)
			if errors.Is(err, dispatch.ErrCandidateChanged) {
				return nil
			}
			return err
		}),
		secret.RunFinalizer(func(
			ctx context.Context,
			tx pgx.Tx,
			finalization secret.RunFinalization,
		) error {
			graph, err := run.LockOwnedFinalization(
				ctx,
				tx,
				run.OwnedFinalizationRequest{
					OrgID:         finalization.OrgID,
					ProjectID:     finalization.ProjectID,
					EnvironmentID: finalization.EnvironmentID,
					RunID:         finalization.RunID,
				},
			)
			if err != nil {
				return err
			}
			_, err = graph.FailCurrentForSecretRevocation(ctx)
			return err
		}),
	)
	if err != nil {
		return fmt.Errorf("configure secret revocation reconciler: %w", err)
	}
	secretRevocationDelivery, err :=
		secret.NewRevocationDeliveryWorker(
			log,
			queries,
			secretRevocationReconciler.ReconcileBatch,
		)
	if err != nil {
		return fmt.Errorf("configure secret revocation delivery: %w", err)
	}
	timerWaitReconciler, err := run.NewTimerWaitReconciler(pool)
	if err != nil {
		return fmt.Errorf("configure timer wait reconciler: %w", err)
	}
	actorReconciler, err := session.NewReconciler(pool)
	if err != nil {
		return fmt.Errorf("configure actor input reconciler: %w", err)
	}
	actorInputDelivery, err := session.NewDeliveryWorker(
		log,
		queries,
		actorReconciler.ReconcileInput,
		actorReconciler.ReconcileClose,
	)
	if err != nil {
		return fmt.Errorf("configure actor input reconciliation delivery: %w", err)
	}
	runWaitDeadlineDelivery, err := run.NewDeadlineWorker(
		log,
		timerWaitReconciler.ReconcileDue,
		tokenWaitReconciler.ReconcileTimeouts,
		actorReconciler.ReconcileTimeouts,
	)
	if err != nil {
		return fmt.Errorf("configure run wait deadline reconciliation delivery: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runners := []func() error{
		func() error { return buildSweeper.Run(runCtx) },
		func() error { return staleWorkerFencer.Run(runCtx) },
		func() error { return queueReconciler.Run(runCtx) },
		func() error { return placementReconciler.Run(runCtx) },
		func() error { return scheduleWorker.Run(runCtx) },
		func() error { return runAdmissionDelivery.Run(runCtx) },
		func() error { return tokenReconcileDelivery.Run(runCtx) },
		func() error { return secretRevocationDelivery.Run(runCtx) },
		func() error { return runWaitDeadlineDelivery.Run(runCtx) },
		func() error { return actorInputDelivery.Run(runCtx) },
		func() error { return telemetryIngestor.Run(runCtx) },
	}
	errc := make(chan error, len(runners))
	var wg sync.WaitGroup
	wg.Add(len(runners))
	for _, runner := range runners {
		go func() {
			defer wg.Done()
			errc <- runner()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	log.Info("Helmr dispatcher running")
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

func newDispatchPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	poolConfig.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
