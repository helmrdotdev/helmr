package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/sqs"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
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
	if err := run(context.Background(), log); err != nil {
		log.Error("control stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadControl()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
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
	scheduleIndex, err := schedule.NewRedisIndex(redisClient)
	if err != nil {
		return fmt.Errorf("configure schedule index: %w", err)
	}
	var asyncPublisher waitpoint.Publisher
	if cfg.AsyncBusURI != "" {
		asyncPublisher, err = sqs.Open(ctx, cfg.AsyncBusURI)
		if err != nil {
			return fmt.Errorf("configure sqs bus: %w", err)
		}
	}
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("parse public URL: %w", err)
	}
	mailer := configuredEmailSender(log, cfg)
	waitpoints, err := waitpoint.NewNotifier(waitpoint.Config{
		Log:        log,
		Store:      queries,
		Mailer:     mailer,
		Publisher:  asyncPublisher,
		PublicURL:  publicURL,
		AuthSecret: []byte(cfg.AuthSecret),
	})
	if err != nil {
		return fmt.Errorf("configure waitpoint notifier: %w", err)
	}
	eventStream, err := control.NewEventStream(log, queries, redisClient)
	if err != nil {
		return fmt.Errorf("configure event stream: %w", err)
	}
	go func() {
		if err := eventStream.RunPublisher(backgroundCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("event stream publisher stopped", "error", err)
			cancelServer()
		}
	}()
	keyring, err := secret.KeyringFromBase64(cfg.SecretEncryptionKey, cfg.SecretEncryptionKeyOld)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	secretStore, err := secret.New(queries, keyring)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	scheduleRunCreator, err := control.NewScheduleRunCreator(log, pool, secretStore, runEnqueuer)
	if err != nil {
		return fmt.Errorf("configure schedule run creator: %w", err)
	}
	scheduleEngine, err := schedule.NewEngine(log, pool, scheduleIndex, scheduleRunCreator, schedule.EngineConfig{
		Jitter: cfg.ScheduleJitter,
	})
	if err != nil {
		return fmt.Errorf("configure schedule engine: %w", err)
	}
	casStore, err := cas.NewS3(ctx, cfg.CASURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	var authProvider control.AuthProvider
	if cfg.GitHubOAuthClientID != "" && cfg.GitHubOAuthClientSecret != "" {
		authProvider = control.NewGitHubOAuthProvider(cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret, publicURL)
	}
	handler, err := control.NewServer(control.ServerConfig{
		Log:                 log,
		DeploymentMode:      cfg.DeploymentMode,
		DB:                  queries,
		TX:                  pool,
		ReadinessDB:         pool,
		Auth:                auth.NewDBAuthenticator(queries),
		CAS:                 casStore,
		Secrets:             secretStore,
		RunEnqueuer:         runEnqueuer,
		DispatchQueue:       dispatchQueue,
		ScheduleEngine:      scheduleEngine,
		Waitpoints:          waitpoints,
		EventStream:         eventStream,
		Mailer:              mailer,
		AuthProvider:        authProvider,
		WorkerTokenSecret:   []byte(cfg.WorkerTokenSigningKey),
		WorkerRegisterToken: cfg.WorkerBootstrapToken,
		SetupToken:          cfg.SetupToken,
		AuthSecret:          []byte(cfg.AuthSecret),
		PublicURL:           publicURL,
		MagicLinkDebugURLs:  cfg.MagicLinkDebugURLs,
	})
	if err != nil {
		return fmt.Errorf("configure control server: %w", err)
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
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
	cancelBackground()
	cancelServer()
	return nil
}

func configuredEmailSender(log *slog.Logger, cfg config.Control) email.Sender {
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
