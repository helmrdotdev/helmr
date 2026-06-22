package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	defaultAddr                = ":8080"
	defaultPublicURL           = "http://127.0.0.1:3000"
	defaultRedisURL            = "redis://127.0.0.1:6379/0"
	defaultAuthSecret          = "helmr-dev-auth-secret-32-byte-value"
	defaultSetupToken          = "dev-setup-token"
	defaultWorkerTokenSecret   = "helmr-dev-worker-token-secret-32"
	defaultSecretEncryptionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	defaultUserID              = "00000000-0000-0000-0000-000000000101"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		log.Error("load dev config", "error", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		log.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := migrate(ctx, pool, cfg.resetDatabase); err != nil {
		log.Error("migrate database", "error", err)
		os.Exit(1)
	}
	if cfg.seedData {
		if err := seedDevData(ctx, pool); err != nil {
			log.Error("seed dev data", "error", err)
			os.Exit(1)
		}
	}
	casStore, err := cas.NewFile(cfg.casDir)
	if err != nil {
		log.Error("configure dev CAS", "error", err)
		os.Exit(1)
	}
	pool.Close()
	pool, err = pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		log.Error("connect database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	queries := db.New(pool)
	redisOptions, err := redis.ParseURL(cfg.redisURL)
	if err != nil {
		log.Error("parse redis URL", "error", err)
		os.Exit(1)
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Error("ping redis", "error", err)
		os.Exit(1)
	}
	eventStream, err := control.NewEventStream(log, queries, redisClient)
	if err != nil {
		log.Error("configure event stream", "error", err)
		os.Exit(1)
	}
	workspaceStreams, err := control.NewWorkspaceStreamNotifier(log, queries, redisClient)
	if err != nil {
		log.Error("configure workspace stream notifier", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := eventStream.RunPublisher(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("event stream publisher stopped", "error", err)
		}
	}()
	go func() {
		if err := workspaceStreams.RunPublisher(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("workspace stream notifier stopped", "error", err)
		}
	}()
	keyring, err := secret.KeyringFromBase64(cfg.secretEncryptionKey, cfg.secretEncryptionKeyOld)
	if err != nil {
		log.Error("load secret encryption key", "error", err)
		os.Exit(1)
	}
	secretStore, err := secret.New(queries, keyring)
	if err != nil {
		log.Error("configure secret store", "error", err)
		os.Exit(1)
	}
	sweeper, err := dispatch.NewExpirySweeper(queries, dispatch.WithExpirySweepLogger(log))
	if err != nil {
		log.Error("configure sweeper", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := sweeper.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sweeper stopped", "error", err)
		}
	}()

	publicURL, err := url.Parse(cfg.publicURL)
	if err != nil {
		log.Error("parse public URL", "error", err)
		os.Exit(1)
	}
	app, err := control.NewServer(control.ServerConfig{
		Log:                 log,
		DeploymentMode:      cfg.deploymentMode,
		DB:                  queries,
		TX:                  pool,
		ReadinessDB:         pool,
		Auth:                auth.NewDBAuthenticator(queries),
		CAS:                 casStore,
		Secrets:             secretStore,
		WorkerTokenSecret:   []byte(cfg.workerTokenSecret),
		WorkerRegisterToken: cfg.workerBootstrapToken,
		SetupToken:          cfg.setupToken,
		AuthSecret:          []byte(cfg.authSecret),
		PublicURL:           publicURL,
		EventStream:         eventStream,
		WorkspaceStreams:    workspaceStreams,
	})
	if err != nil {
		log.Error("configure control server", "error", err)
		os.Exit(1)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dev/login" {
			devLogin(ctx, w, r, pool, queries, cfg)
			return
		}
		app.ServeHTTP(w, r)
	})
	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Info("helmr dev control listening", "addr", cfg.addr, "login_url", strings.TrimRight(cfg.publicURL, "/")+"/dev/login")
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}

type devConfig struct {
	addr                   string
	deploymentMode         string
	databaseURL            string
	redisURL               string
	casDir                 string
	publicURL              string
	authSecret             string
	setupToken             string
	workerBootstrapToken   string
	workerTokenSecret      string
	secretEncryptionKey    string
	secretEncryptionKeyOld string
	resetDatabase          bool
	seedData               bool
}

func loadConfig() (devConfig, error) {
	cfg := devConfig{
		addr:                   env("HELMR_CONTROL_ADDR", defaultAddr),
		deploymentMode:         env("HELMR_DEPLOYMENT_MODE", "self-hosted"),
		databaseURL:            os.Getenv("HELMR_DATABASE_URL"),
		redisURL:               env("HELMR_REDIS_URL", defaultRedisURL),
		casDir:                 env("HELMR_DEV_CAS_DIR", filepath.Join(os.TempDir(), "helmr-dev-cas")),
		publicURL:              env("HELMR_PUBLIC_URL", defaultPublicURL),
		authSecret:             env("HELMR_AUTH_SECRET", defaultAuthSecret),
		setupToken:             env("HELMR_SETUP_TOKEN", defaultSetupToken),
		workerBootstrapToken:   strings.TrimSpace(os.Getenv("HELMR_WORKER_BOOTSTRAP_TOKEN")),
		workerTokenSecret:      env("HELMR_WORKER_TOKEN_SIGNING_KEY", defaultWorkerTokenSecret),
		secretEncryptionKey:    env("HELMR_SECRET_ENCRYPTION_KEY", defaultSecretEncryptionKey),
		secretEncryptionKeyOld: strings.TrimSpace(os.Getenv("HELMR_SECRET_ENCRYPTION_KEY_OLD")),
		resetDatabase:          envBool("HELMR_DEV_RESET_DATABASE"),
		seedData:               envBoolDefault("HELMR_DEV_SEED_DATA", true),
	}
	if cfg.databaseURL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
	}
	if err := auth.ValidateTokenSecret([]byte(cfg.authSecret)); err != nil {
		return cfg, err
	}
	if err := auth.ValidateWorkerTokenSecret([]byte(cfg.workerTokenSecret)); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func env(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes"
}

func envBoolDefault(name string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes"
}

func migrate(ctx context.Context, pool *pgxpool.Pool, reset bool) error {
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		return err
	}
	if serverVersion < 180000 {
		return fmt.Errorf("PostgreSQL 18 or newer is required for uuidv7() defaults; server_version_num=%d", serverVersion)
	}
	if reset {
		if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public`); err != nil {
			return err
		}
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.organizations') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	migrations, err := migrationPaths()
	if err != nil {
		return err
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func migrationPaths() ([]string, error) {
	migrations, err := filepath.Glob("internal/db/schema/migrations/*.up.sql")
	if err != nil {
		return nil, err
	}
	if len(migrations) > 0 {
		return migrations, nil
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if ok {
		sourceRootPattern := filepath.Join(filepath.Dir(sourceFile), "..", "..", "internal", "db", "schema", "migrations", "*.up.sql")
		migrations, err = filepath.Glob(sourceRootPattern)
		if err != nil {
			return nil, err
		}
		if len(migrations) > 0 {
			return migrations, nil
		}
	}

	return nil, fmt.Errorf("no migrations found; run dev/control from the repository root or set cwd to a Helmr source checkout")
}

func devLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, queries *db.Queries, cfg devConfig) {
	userID := mustUUID(defaultUserID)
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, display_name, primary_email)
VALUES ($1, 'Local Developer', 'dev@helmr.local')
ON CONFLICT (id) DO UPDATE
   SET display_name = EXCLUDED.display_name,
       primary_email = EXCLUDED.primary_email,
       disabled_at = NULL,
       updated_at = now()
`, pgvalue.UUID(userID)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, err := auth.GenerateOpaqueToken(32)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hash, err := auth.HashToken([]byte(cfg.authSecret), raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := queries.CreateSession(ctx, db.CreateSessionParams{
		ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		UserID:    pgvalue.UUID(userID),
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(30 * 24 * time.Hour), Valid: true},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "helmr_session_dev",
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func mustUUID(value string) uuid.UUID {
	parsed, err := uuid.Parse(value)
	if err != nil {
		panic(err)
	}
	return parsed
}
