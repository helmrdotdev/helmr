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
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	baseMaxConns        = int32(12)
	runDispatchMaxConns = int32(32)
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
	connectionBudget := baseMaxConns + runDispatchMaxConns
	log.Info("dispatcher database connection budget", "max_connections", connectionBudget,
		"base", baseMaxConns, "run_dispatch", runDispatchMaxConns)
	queries := db.New(pool)
	runDispatchQueries := db.New(runDispatchPool)
	runPlacementStore, err := dispatch.NewRunPlacementStore(runDispatchPool)
	if err != nil {
		return fmt.Errorf("configure run placement store: %w", err)
	}
	runPlacementLaneLock, err := dispatch.NewRunPlacementLaneLock(runDispatchPool)
	if err != nil {
		return fmt.Errorf("configure run placement lane lock: %w", err)
	}
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
	clickHouseClient, err := clickhouse.New(clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
	placementReconciler, err := dispatch.NewPlacementReconciler(
		runPlacementStore, runPlacementLaneLock, runDispatchAuthority,
		runDispatchQueries, runDispatchAuthority,
		runDispatchQueries,
		log,
	)
	if err != nil {
		return fmt.Errorf("configure placement reconciler: %w", err)
	}
	telemetryIngestor, err := telemetry.NewIngestor(log, queries, clickhouse.NewWriter(clickHouseClient))
	if err != nil {
		return fmt.Errorf("configure telemetry ingester: %w", err)
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
	runLeaseRecoveryLock, err := dispatch.NewRunLeaseRecoveryAdvisoryLock(runDispatchPool)
	if err != nil {
		return fmt.Errorf("configure Run lease recovery lock: %w", err)
	}
	runLeaseReconciler, err := dispatch.NewRunLeaseReconciler(runDispatchAuthority, runLeaseRecoveryLock, log)
	if err != nil {
		return fmt.Errorf("configure Run lease reconciler: %w", err)
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
		func() error { return staleWorkerFencer.Run(runCtx) },
		func() error { return runLeaseReconciler.Run(runCtx) },
		func() error { return placementReconciler.Run(runCtx) },
		func() error { return scheduleWorker.Run(runCtx) },
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
