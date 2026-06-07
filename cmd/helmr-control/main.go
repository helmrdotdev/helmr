package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
		case "secrets":
			if err := runSecretsCommand(log, os.Args[2:]); err != nil {
				log.Error("manage secrets", "error", err)
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
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
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
	keyring, err := secret.KeyringFromBase64(cfg.SecretEncryptionKey, cfg.SecretEncryptionKeyOld)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	secretStore, err := secret.New(queries, keyring)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	casStore, err := cas.NewS3(ctx, cfg.CASURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	server := &http.Server{
		Addr: cfg.Addr,
		Handler: control.New(
			log,
			control.WithDBTX(pool),
			control.WithDeploymentMode(cfg.DeploymentMode),
			control.WithCAS(casStore),
			control.WithSecrets(secretStore),
			control.WithRunEnqueuer(runEnqueuer),
			control.WithDispatchQueue(dispatchQueue),
			control.WithAsyncBus(asyncPublisher),
			control.WithRunEventNotifier(runEventNotifier),
			control.WithWorkerAuth(cfg.WorkerTokenSigningKey, 0),
			control.WithDefaultWorkerBootstrapToken(cfg.WorkerBootstrapToken),
			control.WithInitialSetupToken(cfg.SetupToken),
			control.WithUserAuth(cfg.AuthSecret, cfg.PublicURL),
			control.WithMagicLinkDebugURLs(cfg.MagicLinkDebugURLs),
			emailSenderOption(cfg),
			control.WithGitHubOAuth(cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret),
		),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return serverCtx
		},
	}
	shutdownErr := make(chan error, 1)
	go func() {
		<-ctx.Done()
		cancelServer()
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
	cancelServer()
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

func runSecretsCommand(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: helmr-control secrets key-usage|reencrypt [--limit N]")
	}
	cfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	currentKey := strings.TrimSpace(os.Getenv("HELMR_SECRET_ENCRYPTION_KEY"))
	if currentKey == "" {
		return errors.New("HELMR_SECRET_ENCRYPTION_KEY is required")
	}
	oldKey := strings.TrimSpace(os.Getenv("HELMR_SECRET_ENCRYPTION_KEY_OLD"))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, cfg.URL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	queries := db.New(pool)
	keyring, err := secret.KeyringFromBase64(currentKey, oldKey)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	store, err := secret.New(queries, keyring)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	switch args[0] {
	case "key-usage":
		if len(args) != 1 {
			return errors.New("usage: helmr-control secrets key-usage")
		}
		usage, err := store.KeyUsage(ctx)
		if err != nil {
			return err
		}
		for _, row := range usage {
			log.Info("secret key usage", "key_id", row.KeyID, "secret_count", row.SecretCount, "current", row.Current, "old", row.Old)
		}
		return nil
	case "reencrypt":
		limit, err := parseReencryptLimit(args[1:])
		if err != nil {
			return err
		}
		oldKeyID, ok := keyring.OldKeyID()
		if !ok {
			return errors.New("HELMR_SECRET_ENCRYPTION_KEY_OLD is required for secret re-encryption")
		}
		result, err := store.ReencryptBatch(ctx, oldKeyID, limit)
		if err != nil {
			return err
		}
		remaining, err := store.CountByKeyID(ctx, oldKeyID)
		if err != nil {
			return err
		}
		log.Info("secret re-encryption batch complete", "scanned", result.Scanned, "reencrypted", result.Reencrypted, "skipped", result.Skipped, "failed", result.Failed, "remaining_old_key_count", remaining)
		if result.Failed > 0 {
			return fmt.Errorf("%d secrets could not be decrypted with HELMR_SECRET_ENCRYPTION_KEY_OLD", result.Failed)
		}
		return nil
	default:
		return errors.New("usage: helmr-control secrets key-usage|reencrypt [--limit N]")
	}
}

func parseReencryptLimit(args []string) (int32, error) {
	limit := int64(500)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit":
			if i+1 >= len(args) {
				return 0, errors.New("--limit requires a value")
			}
			parsed, err := strconv.ParseInt(args[i+1], 10, 32)
			if err != nil {
				return 0, fmt.Errorf("--limit must be an integer: %w", err)
			}
			limit = parsed
			i++
		default:
			return 0, fmt.Errorf("unknown secrets reencrypt argument %q", args[i])
		}
	}
	if limit <= 0 {
		return 0, errors.New("--limit must be positive")
	}
	return int32(limit), nil
}
