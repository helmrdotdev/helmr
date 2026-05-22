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
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
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

	errc := make(chan error, 2)
	go func() {
		errc <- sweeper.Run(ctx)
	}()
	go func() {
		errc <- queueReconciler.Run(ctx)
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
