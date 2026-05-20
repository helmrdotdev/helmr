package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/runqueue/publisher"
	runqueueredis "github.com/helmrdotdev/helmr/internal/runqueue/redis"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if err := runMigrate(log, os.Args[2:]); err != nil {
				log.Error("migrate database", "error", err)
				os.Exit(1)
			}
			return
		default:
			log.Error("unknown command", "command", os.Args[1])
			os.Exit(1)
		}
	}
	if err := run(log); err != nil {
		log.Error("control stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadControl()
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
	secretKey, err := secret.KeyFromBase64(cfg.SecretEncryptionKey)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	secretStore, err := secret.New(queries, secret.DefaultKeyID, secretKey)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	casStore, err := cas.NewS3(ctx, cfg.CASURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	githubKey, err := githubAppPrivateKey(cfg)
	if err != nil {
		return err
	}
	githubResolver, err := ghapp.NewResolver(cfg.GitHubAppID, cfg.GitHubAppSlug, githubKey)
	if err != nil {
		return fmt.Errorf("configure github app: %w", err)
	}
	server := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(
			log,
			server.WithDBTX(pool),
			server.WithDeploymentMode(cfg.DeploymentMode),
			server.WithGitHubResolver(githubResolver),
			server.WithCAS(casStore),
			server.WithSecrets(secretStore),
			server.WithRunPublisher(runPublisher),
			server.WithRunQueue(runQueue),
			server.WithGitHubWebhookSecret(cfg.GitHubWebhookSecret),
			server.WithWorkerAuth(cfg.WorkerTokenSigningKey, 0),
			server.WithDefaultWorkerRegistrationToken(cfg.WorkerRegistrationToken),
			server.WithInitialSetupToken(cfg.SetupToken),
			server.WithUserAuth(cfg.AuthSecret, cfg.PublicURL),
			server.WithMagicLinkDebugURLs(cfg.MagicLinkDebugURLs),
			magicLinkMailerOption(cfg),
			server.WithGitHubOAuth(cfg.GitHubAppClientID, cfg.GitHubAppClientSecret),
		),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Info("helmr control listening", "addr", cfg.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func magicLinkMailerOption(cfg config.Control) server.Option {
	if cfg.SMTPAddr == "" {
		return func(*server.Server) {}
	}
	return server.WithSMTPMagicLinkMailer(cfg.SMTPAddr, cfg.SMTPUsername, cfg.SMTPPassword, cfg.EmailFrom)
}

func githubAppPrivateKey(cfg config.Control) ([]byte, error) {
	if cfg.GitHubAppPrivateKeyEnv != "" {
		if value := os.Getenv(cfg.GitHubAppPrivateKeyEnv); value != "" {
			return []byte(value), nil
		}
	}
	githubKey, err := os.ReadFile(cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read github app private key: %w", err)
	}
	return githubKey, nil
}

func runMigrate(log *slog.Logger, args []string) error {
	if len(args) != 1 || args[0] != "up" {
		return errors.New("usage: helmr-control migrate up")
	}
	cfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := schema.Up(ctx, cfg.URL); err != nil {
		return err
	}
	log.Info("database migrations are up to date")
	return nil
}
