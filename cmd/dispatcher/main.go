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
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/outbox"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/scheduler"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	baseMaxConns        = int32(12)
	runDispatchMaxConns = int32(32)
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.String())
		return
	}
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
	computerFencingKey, err := disk.NewFencingKey(cfg.ComputerFencingKey)
	if err != nil {
		return fmt.Errorf("configure computer fencing key: %w", err)
	}
	runDispatchAuthority, err := dispatch.NewRunAuthority(
		runDispatchPool,
		computerFencingKey,
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
	staleHostFencer, err := workergroup.NewStaleHostFencer(pool, workergroup.WithStaleHostFenceLogger(log))
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
	scheduleAuthority := definition.NewScheduleAuthority()
	secretStore, err := secret.New(queries, pool, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("configure scheduled Computer CA encryption: %w", err)
	}
	scheduleAdmitter, err := scheduler.NewDBAdmitter(pool, scheduleAuthority, secretStore)
	if err != nil {
		return fmt.Errorf("configure schedule admission: %w", err)
	}
	scheduleWorker, err := scheduler.NewWorker(log, queries, scheduleAdmitter)
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
		secret.ComputerCommandRecoverer(func(
			ctx context.Context,
			candidate secret.ComputerCommandCandidate,
		) error {
			err := runDispatchAuthority.RecoverComputerCommand(
				ctx,
				dispatch.RecoverableComputerCommandCandidate{
					OrgID:            candidate.OrgID,
					CommandID:        candidate.CommandID,
					ComputerID:       candidate.ComputerID,
					ExpectedRevision: candidate.ExpectedRevision,
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
		actorReconciler.ReconcileLifecycle,
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
	controlOutboxLifecycle, err := outbox.NewLifecycle(log, queries)
	if err != nil {
		return fmt.Errorf("configure control outbox lifecycle: %w", err)
	}

	runners := []dispatcherRunner{
		{name: "stale host fencer", run: staleHostFencer.Run},
		{name: "Run lease reconciler", run: runLeaseReconciler.Run},
		{name: "placement reconciler", run: placementReconciler.Run},
		{name: "schedule worker", run: scheduleWorker.Run},
		{name: "token reconciliation delivery", run: tokenReconcileDelivery.Run},
		{name: "secret revocation delivery", run: secretRevocationDelivery.Run},
		{name: "run wait deadline delivery", run: runWaitDeadlineDelivery.Run},
		{name: "actor input delivery", run: actorInputDelivery.Run},
		{name: "telemetry ingestor", run: telemetryIngestor.Run},
		{name: "control outbox lifecycle", run: controlOutboxLifecycle.Run},
	}
	log.Info("Helmr dispatcher running")
	return superviseRunners(ctx, runners)
}

type dispatcherRunner struct {
	name string
	run  func(context.Context) error
}

var errRunnerStoppedUnexpectedly = errors.New("stopped unexpectedly")

// superviseRunners runs every runner until ctx is cancelled or any runner
// exits. A runner that returns nil or context.Canceled while the supervisor
// has not cancelled it is an unexpected exit. The first exit cancels the peers,
// every runner is joined, and all failures are returned together.
func superviseRunners(ctx context.Context, runners []dispatcherRunner) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(runners))
	var wg sync.WaitGroup
	for _, runner := range runners {
		wg.Go(func() {
			err := runner.run(runCtx)
			stopping := runCtx.Err() != nil
			switch {
			case stopping && (err == nil || errors.Is(err, context.Canceled)):
				errc <- nil
			case err == nil:
				errc <- fmt.Errorf("%s: %w", runner.name, errRunnerStoppedUnexpectedly)
			case errors.Is(err, context.Canceled):
				errc <- fmt.Errorf("%s: %w: returned %v", runner.name, errRunnerStoppedUnexpectedly, err)
			default:
				errc <- fmt.Errorf("%s: %w", runner.name, err)
			}
		})
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	cancel()
	wg.Wait()
	close(errc)
	for err := range errc {
		runErr = errors.Join(runErr, err)
	}
	return runErr
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
