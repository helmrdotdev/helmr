package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/asyncbus"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/secret"
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
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
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
	dispatchQueue, err := dispatchredis.New(redisClient)
	if err != nil {
		return fmt.Errorf("configure run queue: %w", err)
	}
	runEnqueuer, err := dispatch.NewEnqueuer(queries, dispatchQueue)
	if err != nil {
		return fmt.Errorf("configure dispatch enqueuer: %w", err)
	}
	var asyncPublisher asyncbus.Publisher
	if cfg.AsyncBusURI != "" {
		asyncPublisher, err = asyncbus.Open(ctx, cfg.AsyncBusURI)
		if err != nil {
			return fmt.Errorf("configure async bus: %w", err)
		}
	}
	runEventNotifier, err := control.NewPostgresRunEventNotifier(backgroundCtx, pool, log)
	if err != nil {
		return fmt.Errorf("configure run event notifier: %w", err)
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
		Handler: control.New(
			log,
			control.WithDBTX(pool),
			control.WithDeploymentMode(cfg.DeploymentMode),
			control.WithGitHubResolver(githubResolver),
			control.WithCAS(casStore),
			control.WithSecrets(secretStore),
			control.WithRunEnqueuer(runEnqueuer),
			control.WithDispatchQueue(dispatchQueue),
			control.WithAsyncBus(asyncPublisher),
			control.WithRunEventNotifier(runEventNotifier),
			control.WithGitHubWebhookSecret(cfg.GitHubWebhookSecret),
			control.WithWorkerAuth(cfg.WorkerTokenSigningKey, 0),
			control.WithDefaultWorkerBootstrapToken(cfg.WorkerBootstrapToken),
			control.WithInitialSetupToken(cfg.SetupToken),
			control.WithUserAuth(cfg.AuthSecret, cfg.PublicURL),
			control.WithMagicLinkDebugURLs(cfg.MagicLinkDebugURLs),
			emailSenderOption(cfg),
			control.WithGitHubOAuth(cfg.GitHubAppClientID, cfg.GitHubAppClientSecret),
		),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr <- server.Shutdown(shutdownCtx)
		cancelBackground()
	}()
	log.Info("helmr control listening", "addr", cfg.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	if ctx.Err() != nil {
		if err := <-shutdownErr; err != nil {
			return fmt.Errorf("shutdown server: %w", err)
		}
	}
	cancelBackground()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelDrain()
	if err := runEventNotifier.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("drain run event notifier: %w", err)
	}
	return nil
}

func emailSenderOption(cfg config.Control) control.Option {
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

func githubAppPrivateKey(cfg config.Control) ([]byte, error) {
	if cfg.GitHubAppPrivateKeyEnv != "" {
		if value := strings.TrimSpace(os.Getenv(cfg.GitHubAppPrivateKeyEnv)); value != "" {
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
