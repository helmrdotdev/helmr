package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/asyncbus"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("dispatcher stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadDispatcher()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

	redisOptions, err := goredis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	redisClient := goredis.NewClient(redisOptions)
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
	scheduleReconcileLock, err := schedule.NewReconcileAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure schedule reconcile lock: %w", err)
	}
	var asyncSubscriber asyncbus.Subscriber
	if cfg.AsyncBusURI != "" {
		asyncSubscriber, err = asyncbus.Open(ctx, cfg.AsyncBusURI)
		if err != nil {
			return fmt.Errorf("configure async bus: %w", err)
		}
	}
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("parse public URL: %w", err)
	}
	mailer := configuredEmailSender(log, cfg)
	notifier, err := waitpoint.NewNotifier(waitpoint.Config{
		Log:        log,
		Store:      queries,
		Mailer:     mailer,
		Publisher:  asyncSubscriber,
		PublicURL:  publicURL,
		AuthSecret: []byte(cfg.AuthSecret),
	})
	if err != nil {
		return fmt.Errorf("configure waitpoint notifier: %w", err)
	}
	notificationWorker := waitpoint.NewWorker(notifier, asyncSubscriber, log)
	scheduleRunCreator, err := control.NewScheduleRunCreator(log, pool, secretStore, enqueuer)
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
	errc := make(chan error, 4)
	var wg sync.WaitGroup
	wg.Add(4)
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
		errc <- notificationWorker.Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		errc <- scheduleWorker.Run(runCtx)
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

func configuredEmailSender(log *slog.Logger, cfg config.Dispatcher) email.Sender {
	switch cfg.EmailProvider {
	case config.EmailProviderSMTP:
		return email.NewSMTPSender(cfg.SMTPAddr, cfg.SMTPUsername, cfg.SMTPPassword, cfg.EmailFrom)
	case config.EmailProviderResend:
		return email.NewResendSender(cfg.ResendAPIKey, cfg.EmailFrom)
	case config.EmailProviderLog:
		return email.LogSender{Log: log}
	default:
		return email.Unconfigured{}
	}
}
