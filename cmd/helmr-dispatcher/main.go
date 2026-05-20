package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatcher"
	"github.com/helmrdotdev/helmr/internal/runqueue/publisher"
	runqueueredis "github.com/helmrdotdev/helmr/internal/runqueue/redis"
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
	queries := db.New(pool)

	redisOptions, err := goredis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	redisClient := goredis.NewClient(redisOptions)
	defer redisClient.Close()
	runQueue, err := runqueueredis.New(redisClient)
	if err != nil {
		return fmt.Errorf("configure run queue: %w", err)
	}
	runPublisher, err := publisher.New(queries, runQueue)
	if err != nil {
		return fmt.Errorf("configure run queue publisher: %w", err)
	}

	sweeperLock, err := dispatcher.NewSweeperAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure sweeper lock: %w", err)
	}
	sweeper, err := dispatcher.NewSweeper(
		queries,
		dispatcher.WithLogger(log),
		dispatcher.WithSweepLock(sweeperLock),
	)
	if err != nil {
		return fmt.Errorf("configure sweeper: %w", err)
	}
	runQueueReconcileLock, err := dispatcher.NewRunQueueReconcileAdvisoryLock(pool)
	if err != nil {
		return fmt.Errorf("configure run queue reconcile lock: %w", err)
	}
	runQueueReconciler, err := dispatcher.NewRunQueueReconciler(
		queries,
		runPublisher,
		dispatcher.WithRunQueueReconcileLogger(log),
		dispatcher.WithRunQueueReconcileLock(runQueueReconcileLock),
	)
	if err != nil {
		return fmt.Errorf("configure run queue reconciler: %w", err)
	}

	errc := make(chan error, 2)
	go func() {
		errc <- sweeper.Run(ctx)
	}()
	go func() {
		errc <- runQueueReconciler.Run(ctx)
	}()

	log.Info("helmr dispatcher running")
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}
