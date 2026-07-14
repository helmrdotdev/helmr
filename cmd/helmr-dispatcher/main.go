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
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"

	"github.com/helmrdotdev/helmr/internal/fleet"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	baseMaxConns          = int32(12)
	runDispatchMaxConns   = int32(4)
	buildDispatchMaxConns = int32(2)
	fleetSourceMaxConns   = int32(2)
	fleetLockMaxConns     = int32(2)
)

var loadAWSConfig = awsconfig.LoadDefaultConfig

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
	fleetControllers, fleetPools, err := configureFleetControllers(ctx, cfg)
	if err != nil {
		return err
	}
	for _, fleetPool := range fleetPools {
		defer fleetPool.Close()
	}
	connectionBudget := baseMaxConns + runDispatchMaxConns + buildDispatchMaxConns
	if len(fleetPools) > 0 {
		connectionBudget += fleetLockMaxConns + int32(len(fleetPools)-1)*fleetSourceMaxConns
	}
	log.Info("dispatcher database connection budget", "max_connections", connectionBudget,
		"base", baseMaxConns, "run_dispatch", runDispatchMaxConns, "build_dispatch", buildDispatchMaxConns,
		"fleet_controllers", len(fleetControllers))
	queries := db.New(pool)
	runDispatchQueries := db.New(runDispatchPool)
	buildDispatchQueries := db.New(buildDispatchPool)
	executionAuthority, err := dispatch.NewAuthority(pool)
	if err != nil {
		return fmt.Errorf("configure execution authority: %w", err)
	}
	runDispatchAuthority, err := dispatch.NewAuthority(runDispatchPool)
	if err != nil {
		return fmt.Errorf("configure run dispatch authority: %w", err)
	}
	buildDispatchAuthority, err := dispatch.NewAuthority(buildDispatchPool)
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
	telemetryReader := telemetry.NewHistoricalReader(clickHouseClient)

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
		queue, wakePublisher, log,
	)
	if err != nil {
		return fmt.Errorf("configure placement reconciler: %w", err)
	}
	checkpointReconciler, err := dispatch.NewCheckpointReconciler(queries, executionAuthority, wakePublisher, log)
	if err != nil {
		return fmt.Errorf("configure checkpoint reconciler: %w", err)
	}
	runEnqueuer, err := dispatch.NewEnqueuer(runDispatchQueries, queue)
	if err != nil {
		return fmt.Errorf("configure run dispatch enqueuer: %w", err)
	}
	buildEnqueuer, err := dispatch.NewEnqueuer(buildDispatchQueries, queue)
	if err != nil {
		return fmt.Errorf("configure build dispatch enqueuer: %w", err)
	}
	eventStream, err := control.NewEventStream(log, queries, redisClient, control.EventStreamConfig{
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
	workerGroupFenceGrace := make(map[string]dispatch.WorkerGroupFenceGrace, len(cfg.WorkerFleets))
	for _, configuredFleet := range cfg.WorkerFleets {
		workerGroupFenceGrace[configuredFleet.GroupID] = dispatch.WorkerGroupFenceGrace{
			Observation: configuredFleet.StaleWorkerTimeout, Registration: configuredFleet.ReadinessTimeout,
		}
	}
	staleWorkerFencer, err := dispatch.NewStaleWorkerFencer(staleWorkerTransactions,
		dispatch.WithStaleWorkerFenceLock(staleWorkerLock),
		dispatch.WithWorkerGroupFenceGrace(workerGroupFenceGrace),
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
	)
	if err != nil {
		return fmt.Errorf("configure queue reconciler: %w", err)
	}
	preparedRuntimeWarmLock, err := dispatch.NewRuntimePrepareAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure prepared runtime warm lock: %w", err)
	}
	preparedRuntimeWarmer, err := dispatch.NewRuntimePreparer(
		executionAuthority,
		dispatch.WithRuntimePrepareLogger(log),
		dispatch.WithRuntimePrepareLock(preparedRuntimeWarmLock),
		dispatch.WithRuntimePrepareTarget(int32(cfg.RuntimePrepareTarget)),
		dispatch.WithRuntimePrepareLimit(int32(cfg.RuntimePrepareLimit)),
		dispatch.WithRuntimePrepareInterval(cfg.RuntimePrepareEvery),
		dispatch.WithRuntimePrepareWakePublisher(wakePublisher),
	)
	if err != nil {
		return fmt.Errorf("configure prepared runtime warmer: %w", err)
	}
	scheduleReconcileLock, err := schedule.NewReconcileAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure schedule reconcile lock: %w", err)
	}
	scheduleRunCreator, err := control.NewScheduleRunCreator(log, pool, secretStore, runEnqueuer, eventStream)
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
	runners := []func() error{
		func() error { return sweeper.Run(runCtx) },
		func() error { return buildSweeper.Run(runCtx) },
		func() error { return staleWorkerFencer.Run(runCtx) },
		func() error { return queueReconciler.Run(runCtx) },
		func() error { return placementReconciler.Run(runCtx) },
		func() error { return checkpointReconciler.Run(runCtx) },
		func() error { return preparedRuntimeWarmer.Run(runCtx) },
		func() error { return scheduleWorker.Run(runCtx) },
		func() error { return telemetryIngestor.Run(runCtx) },
	}
	for _, controller := range fleetControllers {
		runners = append(runners, func() error { return controller.Run(runCtx) })
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

func configureFleetControllers(ctx context.Context, cfg config.Dispatcher) ([]*fleet.Controller, []*pgxpool.Pool, error) {
	if len(cfg.WorkerFleets) == 0 {
		return nil, nil, nil
	}
	awsCfg, err := loadAWSConfig(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load AWS config for worker fleet controller: %w", err)
	}
	groups := make(map[string]string, len(cfg.WorkerFleets))
	roles := make(map[string]struct{}, 2)
	for _, configured := range cfg.WorkerFleets {
		groups[configured.GroupID] = configured.ASGName
		roles[configured.Role] = struct{}{}
	}
	actuator, err := fleet.NewAWSActuator(autoscaling.NewFromConfig(awsCfg), groups)
	if err != nil {
		return nil, nil, fmt.Errorf("configure AWS fleet actuator: %w", err)
	}
	cloudWatchClient := cloudwatch.NewFromConfig(awsCfg)
	pools := make([]*pgxpool.Pool, 0, len(roles)+1)
	closePools := func() {
		for _, pool := range pools {
			pool.Close()
		}
	}
	lockPool, err := newDispatchPool(ctx, cfg.DatabaseURL, fleetLockMaxConns)
	if err != nil {
		return nil, nil, fmt.Errorf("configure fleet leader database pool: %w", err)
	}
	pools = append(pools, lockPool)
	leaders, err := fleet.NewPGLeaderElector(lockPool)
	if err != nil {
		closePools()
		return nil, nil, fmt.Errorf("configure fleet leaders: %w", err)
	}
	sourcePools := make(map[string]*pgxpool.Pool, len(roles))
	for _, role := range []string{"run", "build"} {
		if _, exists := roles[role]; !exists {
			continue
		}
		pool, err := newDispatchPool(ctx, cfg.DatabaseURL, fleetSourceMaxConns)
		if err != nil {
			closePools()
			return nil, nil, fmt.Errorf("configure %s fleet source database pool: %w", role, err)
		}
		pools = append(pools, pool)
		sourcePools[role] = pool
	}
	controllers := make([]*fleet.Controller, 0, len(cfg.WorkerFleets))
	for _, configured := range cfg.WorkerFleets {
		capacity := fleet.Capacity{
			MilliCPU: configured.MilliCPU, MemoryBytes: configured.MemoryBytes,
			WorkloadDiskBytes: configured.WorkloadDiskBytes, ScratchBytes: configured.ScratchBytes,
			BuildCacheBytes: configured.BuildCacheBytes, ArtifactCacheBytes: configured.ArtifactCacheBytes,
			VMSlots: configured.VMSlots, BuildExecutors: configured.BuildExecutors,
		}
		planner, err := fleet.NewPlanner(fleet.Policy{
			MinWorkers: configured.MinWorkers, WarmWorkers: configured.WarmWorkers, MaxWorkers: configured.MaxWorkers,
			InstanceCapacity: capacity, AllowedCompatibilityKeys: configured.CompatibilityKeys,
			MaxScaleOutPerCycle: configured.MaxScaleOutPerCycle, MaxPendingWorkers: configured.MaxPending,
			MaxPackingItems: configured.MaxPackingItems, ScaleOutCooldown: configured.ScaleOutCooldown,
			ScaleInCooldown: configured.ScaleInCooldown, ScaleInHysteresis: configured.ScaleInHysteresis,
			EmergencyStop: configured.EmergencyStop,
		})
		if err != nil {
			closePools()
			return nil, nil, fmt.Errorf("configure fleet planner for %q: %w", configured.GroupID, err)
		}
		queryTimeout := min(5*time.Second, configured.ControllerInterval)
		source, err := fleet.NewPostgresSource(sourcePools[configured.Role], configured.Role, capacity, configured.QueuedRunScratchBytes, queryTimeout)
		if err != nil {
			closePools()
			return nil, nil, fmt.Errorf("configure fleet source for %q: %w", configured.GroupID, err)
		}
		publisher, err := fleet.NewCloudWatchPublisher(cloudWatchClient, cfg.FleetMetricsNamespace, configured.Role, configured.MetricsInterval)
		if err != nil {
			closePools()
			return nil, nil, fmt.Errorf("configure fleet metrics for %q: %w", configured.GroupID, err)
		}
		controller, err := fleet.NewController(fleet.ControllerConfig{
			GroupID: configured.GroupID, Interval: configured.ControllerInterval,
			InitialBackoff: configured.ControllerInterval, MaxBackoff: max(30*time.Second, configured.ControllerInterval),
			MetricsTimeout: min(2*time.Second, configured.ControllerInterval), OperationTimeout: 30 * time.Second,
			DrainTimeout: configured.DrainTimeout,
		}, planner, source, leaders, actuator, publisher, nil, nil)
		if err != nil {
			closePools()
			return nil, nil, fmt.Errorf("configure fleet controller for %q: %w", configured.GroupID, err)
		}
		controllers = append(controllers, controller)
	}
	return controllers, pools, nil
}
