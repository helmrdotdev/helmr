package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/asyncbus"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
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
	secretKey, err := secret.KeyFromBase64(cfg.SecretEncryptionKey)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	secretStore, err := secret.New(queries, secret.DefaultKeyID, secretKey)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	githubKey, err := githubAppPrivateKey(cfg)
	if err != nil {
		return err
	}
	githubResolver, err := ghapp.NewResolver(cfg.GitHubAppID, cfg.GitHubAppSlug, githubKey)
	if err != nil {
		return fmt.Errorf("configure github app: %w", err)
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
	var asyncSubscriber asyncbus.Subscriber
	if cfg.AsyncBusURI != "" {
		asyncSubscriber, err = asyncbus.Open(ctx, cfg.AsyncBusURI)
		if err != nil {
			return fmt.Errorf("configure async bus: %w", err)
		}
	}
	notificationWorker, err := control.NewWaitpointNotificationWorker(
		log,
		queries,
		asyncSubscriber,
		control.WithUserAuth(cfg.AuthSecret, cfg.PublicURL),
		dispatcherEmailSenderOption(cfg),
	)
	if err != nil {
		return fmt.Errorf("configure waitpoint notification worker: %w", err)
	}
	scheduleRunCreator, err := control.NewScheduleRunCreator(log, pool, githubResolver, secretStore, enqueuer)
	if err != nil {
		return fmt.Errorf("configure schedule run creator: %w", err)
	}
	scheduleWorker, err := schedule.NewWorker(log, pool, scheduleRunCreator)
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

func dispatcherEmailSenderOption(cfg config.Dispatcher) control.Option {
	switch cfg.EmailProvider {
	case config.EmailProviderSMTP:
		return control.WithSMTPEmailSender(cfg.SMTPAddr, cfg.SMTPUsername, cfg.SMTPPassword, cfg.EmailFrom)
	case config.EmailProviderResend:
		return control.WithResendEmailSender(cfg.ResendAPIKey, cfg.EmailFrom)
	case config.EmailProviderLog:
		return control.WithLogEmailSender()
	default:
		return control.WithDisabledEmailSender()
	}
}

func githubAppPrivateKey(cfg config.Dispatcher) ([]byte, error) {
	if cfg.GitHubAppPrivateKeyEnv != "" {
		if value := os.Getenv(cfg.GitHubAppPrivateKeyEnv); strings.TrimSpace(value) != "" {
			return []byte(value), nil
		}
	}
	githubKey, err := os.ReadFile(cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read github app private key: %w", err)
	}
	return githubKey, nil
}
