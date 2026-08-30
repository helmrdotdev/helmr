package main

import (
	"context"
	"encoding/base64"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bootstrap"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	clickhouseschema "github.com/helmrdotdev/helmr/internal/clickhouse/schema"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/controlplane"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/eventstream"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	defaultAddr                = ":8080"
	defaultPublicURL           = "http://127.0.0.1:3000"
	defaultRedisURL            = "redis://127.0.0.1:6379/0"
	defaultAuthKey             = "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ="
	defaultSetupToken          = "dev-setup-token"
	defaultWorkerTokenKey      = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	defaultSecretEncryptionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	defaultWorkspaceFencingKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	defaultTokenCredentialKey  = "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="
	defaultUserID              = "00000000-0000-7000-8000-000000000101"
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
	if err := bootstrap.Apply(ctx, pool, bootstrap.Config{
		Enabled: cfg.bootstrap.Enabled, RegionID: cfg.bootstrap.RegionID,
		RegionDisplayName: cfg.bootstrap.RegionDisplayName, RegionLocation: cfg.bootstrap.RegionLocation,
		WorkerGroupName: cfg.bootstrap.WorkerGroupName, WorkerToken: cfg.bootstrap.WorkerToken,
	}); err != nil {
		log.Error("bootstrap platform", "error", err)
		os.Exit(1)
	}
	if cfg.seedData {
		if err := seedDevData(ctx, pool, cfg); err != nil {
			log.Error("seed dev data", "error", err)
			os.Exit(1)
		}
	}
	casStore, err := cas.NewFile(cfg.casDir)
	if err != nil {
		log.Error("configure dev CAS", "error", err)
		os.Exit(1)
	}
	runtimeRaw, err := os.ReadFile(cfg.deploymentRuntimeDescriptorPath)
	if err != nil {
		log.Error("read dev deployment Runtime descriptor", "error", err)
		os.Exit(1)
	}
	runtimeDescriptor, err := deployment.ParseRuntimeDescriptor(runtimeRaw)
	if err != nil {
		log.Error("parse dev deployment Runtime descriptor", "error", err)
		os.Exit(1)
	}
	bundleAdmission := deployment.DeploymentBundleAdmission{Runtime: runtimeDescriptor}
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
	clickHouseConfig := clickhouse.Config{
		URL:      cfg.clickHouseURL,
		User:     cfg.clickHouseUser,
		Password: cfg.clickHousePassword,
	}
	if err := clickhouseschema.Up(ctx, clickHouseConfig); err != nil {
		log.Error("migrate clickhouse", "error", err)
		os.Exit(1)
	}
	clickHouseClient, err := clickhouse.New(clickHouseConfig)
	if err != nil {
		log.Error("configure clickhouse", "error", err)
		os.Exit(1)
	}
	defer clickHouseClient.Close()
	telemetryReader := clickhouse.NewReader(clickHouseClient)
	eventStream, err := eventstream.New(log, queries, redisClient, eventstream.Config{
		TelemetryReader: telemetryReader,
	})
	if err != nil {
		log.Error("configure event stream", "error", err)
		os.Exit(1)
	}
	telemetryIngestor, err := telemetry.NewIngestor(log, queries, clickhouse.NewWriter(clickHouseClient))
	if err != nil {
		log.Error("configure telemetry ingester", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := eventStream.RunPublisher(ctx); !errors.Is(err, context.Canceled) {
			log.Error("event stream publisher stopped", "error", err)
		}
	}()
	go func() {
		if err := telemetryIngestor.Run(ctx); !errors.Is(err, context.Canceled) {
			log.Error("telemetry ingester stopped", "error", err)
		}
	}()
	secretStore, err := secret.New(queries, pool, cfg.encryptionKey)
	if err != nil {
		log.Error("configure secret store", "error", err)
		os.Exit(1)
	}
	workspaceFencingKey, err := workspace.NewFencingKey(cfg.workspaceFencingKey)
	if err != nil {
		log.Error("configure Workspace fencing key", "error", err)
		os.Exit(1)
	}
	tokenCredentialKey, err := auth.NewCredentialKey(cfg.tokenCredentialKey)
	if err != nil {
		log.Error("configure Token credential key", "error", err)
		os.Exit(1)
	}
	publicURL, err := url.Parse(cfg.publicURL)
	if err != nil {
		log.Error("parse public URL", "error", err)
		os.Exit(1)
	}
	app, err := controlplane.NewServer(controlplane.ServerConfig{
		Log:                   log,
		DeploymentMode:        cfg.deploymentMode,
		DB:                    queries,
		TX:                    pool,
		ReadinessDB:           pool,
		Auth:                  controlplane.NewDBAuthenticator(queries),
		CAS:                   casStore,
		BundleAdmission:       &bundleAdmission,
		PlatformStore:         casStore,
		Secrets:               secretStore,
		SecretDelivery:        secretStore,
		WorkspaceFencingKey:   workspaceFencingKey,
		TokenCredentialKey:    tokenCredentialKey,
		WorkerTokenSigningKey: cfg.workerTokenKey,
		SetupToken:            cfg.setupToken,
		AuthKey:               cfg.authKey,
		PublicURL:             publicURL,
		APIOrigin:             publicURL,
		EventStream:           eventStream,
		TelemetryReader:       telemetryReader,
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

	log.Info("Helmr dev control listening", "addr", cfg.addr, "login_url", strings.TrimRight(cfg.publicURL, "/")+"/dev/login")
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}

type devConfig struct {
	addr                            string
	deploymentMode                  string
	databaseURL                     string
	bootstrap                       config.Bootstrap
	clickHouseURL                   string
	clickHouseUser                  string
	clickHousePassword              string
	redisURL                        string
	casDir                          string
	deploymentRuntimeDescriptorPath string
	publicURL                       string
	authKey                         []byte
	setupToken                      string
	workerTokenKey                  []byte
	encryptionKey                   []byte
	workspaceFencingKey             []byte
	tokenCredentialKey              []byte
	resetDatabase                   bool
	seedData                        bool
}

func loadConfig() (devConfig, error) {
	bootstrapConfig, err := config.LoadBootstrap()
	if err != nil {
		return devConfig{}, err
	}
	cfg := devConfig{
		addr:                            textEnv("CONTROL_PLANE_ADDR", defaultAddr),
		deploymentMode:                  textEnv("DEPLOYMENT_MODE", "self-hosted"),
		databaseURL:                     textEnv("DATABASE_URL", ""),
		bootstrap:                       bootstrapConfig,
		clickHouseURL:                   textEnv("CLICKHOUSE_URL", ""),
		clickHouseUser:                  textEnv("CLICKHOUSE_USER", ""),
		clickHousePassword:              secretEnv("CLICKHOUSE_PASSWORD", ""),
		redisURL:                        textEnv("REDIS_URL", defaultRedisURL),
		casDir:                          textEnv("HELMR_DEV_CAS_DIR", filepath.Join(os.TempDir(), "helmr-dev-cas")),
		deploymentRuntimeDescriptorPath: textEnv("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH", ""),
		publicURL:                       textEnv("PUBLIC_URL", defaultPublicURL),
		setupToken:                      secretEnv("SETUP_TOKEN", defaultSetupToken),
	}
	if cfg.resetDatabase, err = boolEnv("HELMR_DEV_RESET_DATABASE", false); err != nil {
		return cfg, err
	}
	if cfg.seedData, err = boolEnv("HELMR_DEV_SEED_DATA", true); err != nil {
		return cfg, err
	}
	for _, key := range []struct {
		name     string
		fallback string
		target   *[]byte
	}{
		{name: "AUTH_KEY", fallback: defaultAuthKey, target: &cfg.authKey},
		{name: "WORKER_TOKEN_SIGNING_KEY", fallback: defaultWorkerTokenKey, target: &cfg.workerTokenKey},
		{name: "ENCRYPTION_KEY", fallback: defaultSecretEncryptionKey, target: &cfg.encryptionKey},
		{name: "WORKSPACE_FENCING_KEY", fallback: defaultWorkspaceFencingKey, target: &cfg.workspaceFencingKey},
		{name: "TOKEN_CREDENTIAL_KEY", fallback: defaultTokenCredentialKey, target: &cfg.tokenCredentialKey},
	} {
		*key.target, err = decodeRootKey(key.name, secretEnv(key.name, key.fallback))
		if err != nil {
			return cfg, err
		}
	}
	if cfg.databaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.deploymentRuntimeDescriptorPath == "" {
		return cfg, errors.New("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH is required")
	}
	if strings.TrimSpace(cfg.setupToken) != cfg.setupToken {
		return cfg, errors.New("SETUP_TOKEN must not have surrounding whitespace")
	}
	if cfg.clickHouseURL == "" {
		return cfg, errors.New("CLICKHOUSE_URL is required")
	}
	return cfg, nil
}

func decodeRootKey(name, encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64: %w", name, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to exactly 32 bytes, got %d", name, len(key))
	}
	if base64.StdEncoding.EncodeToString(key) != encoded {
		return nil, fmt.Errorf("%s must use canonical base64", name)
	}
	return key, nil
}

func textEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func secretEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func boolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool, reset bool) error {
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		return err
	}
	if serverVersion < 180000 {
		return fmt.Errorf("PostgreSQL 18 or newer is required by the Helmr schema baseline; server_version_num=%d", serverVersion)
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
		sourceRootPattern := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "internal", "db", "schema", "migrations", "*.up.sql")
		migrations, err = filepath.Glob(sourceRootPattern)
		if err != nil {
			return nil, err
		}
		if len(migrations) > 0 {
			return migrations, nil
		}
	}

	return nil, fmt.Errorf("no migrations found; run cmd/internal/dev-controlplane from the repository root or set cwd to a Helmr source checkout")
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
	raw, err := auth.GenerateOpaque(32)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	authKeys, err := auth.NewKeys(cfg.authKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hash, err := auth.HashToken(authKeys.Session, raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := queries.CreateAuthSession(ctx, db.CreateAuthSessionParams{
		ID:        pgvalue.UUID(uuid.NewV7()),
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
