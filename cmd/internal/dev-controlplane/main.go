package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	clickhouseschema "github.com/helmrdotdev/helmr/internal/clickhouse/schema"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/controlplane"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/eventstream"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	defaultAddr                    = ":8080"
	defaultPublicURL               = "http://127.0.0.1:3000"
	defaultRedisURL                = "redis://127.0.0.1:6379/0"
	defaultAuthKey                 = "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ="
	defaultSetupToken              = "dev-setup-token"
	defaultWorkerHostCredentialKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	defaultSecretEncryptionKey     = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	defaultComputerFencingKey      = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	defaultTokenCredentialKey      = "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="
	defaultUserID                  = "00000000-0000-7000-8000-000000000101"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := runDev(context.Background(), log); err != nil {
		log.Error("Helmr dev control stopped", "error", err)
		os.Exit(1)
	}
}

func runDev(ctx context.Context, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Restore default signal handling after the first signal so a second one
	// kills the process during a long drain or loop join.
	context.AfterFunc(ctx, stop)

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load dev config: %w", err)
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	pool, err := dbpool.New(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	freshInit, err := migrate(ctx, pool)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if cfg.bootstrap.Enabled {
		if err := workergroup.Bootstrap(ctx, pool, workergroup.BootstrapConfig{
			RegionID: cfg.bootstrap.RegionID, RegionDisplayName: cfg.bootstrap.RegionDisplayName,
			RegionLocation: cfg.bootstrap.RegionLocation, GroupName: cfg.bootstrap.WorkerGroupName,
			EnrollmentToken: cfg.bootstrap.WorkerToken,
		}); err != nil {
			return fmt.Errorf("bootstrap platform: %w", err)
		}
	}
	if cfg.seedData && freshInit {
		if err := seedDevData(ctx, pool, cfg); err != nil {
			return fmt.Errorf("seed dev data: %w", err)
		}
	}
	casStore, err := cass3.New(ctx, cfg.casURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	platformStore, err := cass3.NewImmutable(ctx, cfg.platformStoreURI)
	if err != nil {
		return fmt.Errorf("configure platform artifact store: %w", err)
	}
	runtimeRaw, err := os.ReadFile(cfg.deploymentRuntimeDescriptorPath)
	if err != nil {
		return fmt.Errorf("read dev deployment Runtime descriptor: %w", err)
	}
	runtimeDescriptor, err := artifact.ParseRuntimeDescriptor(runtimeRaw)
	if err != nil {
		return fmt.Errorf("parse dev deployment Runtime descriptor: %w", err)
	}
	bundleAdmission := bundle.Admission{Runtime: runtimeDescriptor}
	pool.Close()
	pool, err = dbpool.New(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	queries := db.New(pool)
	clickHouseConfig := clickhouse.Config{
		URL:      cfg.clickHouseURL,
		User:     cfg.clickHouseUser,
		Password: cfg.clickHousePassword,
	}
	if err := clickhouseschema.Up(ctx, clickHouseConfig); err != nil {
		return fmt.Errorf("migrate clickhouse: %w", err)
	}
	clickHouseClient, err := clickhouse.New(clickHouseConfig)
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
	redisOptions, err := redis.ParseURL(cfg.redisURL)
	if err != nil {
		return fmt.Errorf("parse redis URL: %w", err)
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	telemetryReader := clickhouse.NewReader(clickHouseClient)
	eventStream, err := eventstream.New(log, queries, redisClient, eventstream.Config{
		TelemetryReader: telemetryReader,
	})
	if err != nil {
		return fmt.Errorf("configure event stream: %w", err)
	}
	telemetryIngestor, err := telemetry.NewIngestor(log, queries, clickhouse.NewWriter(clickHouseClient))
	if err != nil {
		return fmt.Errorf("configure telemetry ingester: %w", err)
	}
	secretStore, err := secret.New(queries, pool, cfg.encryptionKey)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	computerFencingKey, err := disk.NewFencingKey(cfg.computerFencingKey)
	if err != nil {
		return fmt.Errorf("configure Computer fencing key: %w", err)
	}
	tokenCredentialKey, err := auth.NewCredentialKey(cfg.tokenCredentialKey)
	if err != nil {
		return fmt.Errorf("configure Token credential key: %w", err)
	}
	publicURL, err := url.Parse(cfg.publicURL)
	if err != nil {
		return fmt.Errorf("parse public URL: %w", err)
	}
	// This development-only entrypoint uses local roots even when exercising
	// managed-cloud console behavior. Production composition lives in control-plane.
	computerKeys, err := computerkey.NewLocal("dev-computer-root", cfg.computerWrappingKey)
	if err != nil {
		return fmt.Errorf("configure Computer wrapping key: %w", err)
	}
	app, err := controlplane.NewServer(controlplane.ServerConfig{
		ComputerKeys:                   computerKeys,
		Log:                            log,
		DeploymentMode:                 cfg.deploymentMode,
		DB:                             queries,
		TX:                             pool,
		ReadinessDB:                    pool,
		Auth:                           identity.NewAPIKeyAuthenticator(queries),
		CAS:                            casStore,
		BundleAdmission:                bundleAdmission,
		PlatformStore:                  platformStore,
		Secrets:                        secretStore,
		SecretDelivery:                 secretStore,
		SecretProxy:                    secretStore,
		ComputerFencingKey:             computerFencingKey,
		TokenCredentialKey:             tokenCredentialKey,
		WorkerHostCredentialSigningKey: cfg.workerHostCredentialKey,
		SetupToken:                     cfg.setupToken,
		AuthKey:                        cfg.authKey,
		PublicURL:                      publicURL,
		APIOrigin:                      publicURL,
		EventStream:                    eventStream,
		TelemetryReader:                telemetryReader,
	})
	if err != nil {
		return fmt.Errorf("configure control server: %w", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dev/login" {
			devLogin(w, r, pool, queries, cfg)
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
	loops := []backgroundLoop{
		{name: "event stream publisher", run: eventStream.RunPublisher},
		{name: "telemetry ingester", run: telemetryIngestor.Run},
	}
	loginURL := strings.TrimRight(cfg.publicURL, "/") + "/dev/login"
	// Deferred Redis, ClickHouse and PostgreSQL closes run only after serveDev
	// has drained or force-closed HTTP and joined every background loop.
	return serveDev(ctx, log, httpServer, loginURL, loops, 10*time.Second)
}

type backgroundLoop struct {
	name string
	run  func(context.Context) error
}

// serveDev serves until ctx is done or the server fails, then awaits the HTTP
// drain before it cancels and joins the background loops.
func serveDev(
	ctx context.Context,
	log *slog.Logger,
	server *http.Server,
	loginURL string,
	loops []backgroundLoop,
	shutdownTimeout time.Duration,
) error {
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	server.BaseContext = func(net.Listener) context.Context {
		return serverCtx
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	loopCtx, cancelLoops := context.WithCancel(context.Background())
	var loopWG sync.WaitGroup
	for _, loop := range loops {
		loopWG.Go(func() {
			if err := loop.run(loopCtx); !errors.Is(err, context.Canceled) {
				log.Error("background loop stopped", "loop", loop.name, "error", err)
			}
		})
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(listener)
	}()
	log.Info("Helmr dev control listening", "addr", listener.Addr().String(), "login_url", loginURL)

	var runErr error
	serverStopped := false
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		serverStopped = true
		runErr = fmt.Errorf("serve: %w", err)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancelShutdown()
	cancelServer()
	if shutdownErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shutdown server: %w", shutdownErr))
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("close server: %w", err))
		}
	}
	if !serverStopped {
		if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("serve: %w", err))
		}
	}
	cancelLoops()
	loopWG.Wait()
	return runErr
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
	casURI                          string
	platformStoreURI                string
	deploymentRuntimeDescriptorPath string
	publicURL                       string
	authKey                         []byte
	setupToken                      string
	workerHostCredentialKey         []byte
	encryptionKey                   []byte
	computerWrappingKey             []byte
	computerFencingKey              []byte
	tokenCredentialKey              []byte
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
		casURI:                          textEnv("CAS_URI", ""),
		platformStoreURI:                textEnv("PLATFORM_STORE_URI", ""),
		deploymentRuntimeDescriptorPath: textEnv("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH", ""),
		publicURL:                       textEnv("PUBLIC_URL", defaultPublicURL),
		setupToken:                      secretEnv("SETUP_TOKEN", defaultSetupToken),
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
		{name: "WORKER_HOST_CREDENTIAL_SIGNING_KEY", fallback: defaultWorkerHostCredentialKey, target: &cfg.workerHostCredentialKey},
		{name: "COMPUTER_WRAPPING_KEY", fallback: "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=", target: &cfg.computerWrappingKey},
		{name: "ENCRYPTION_KEY", fallback: defaultSecretEncryptionKey, target: &cfg.encryptionKey},
		{name: "COMPUTER_FENCING_KEY", fallback: defaultComputerFencingKey, target: &cfg.computerFencingKey},
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
	if cfg.casURI == "" {
		return cfg, errors.New("CAS_URI is required")
	}
	if cfg.platformStoreURI == "" {
		return cfg, errors.New("PLATFORM_STORE_URI is required")
	}
	if err := cass3.ValidateDistinctS3Stores(cfg.casURI, cfg.platformStoreURI); err != nil {
		return cfg, err
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

func migrate(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		return false, err
	}
	if serverVersion < 180000 {
		return false, fmt.Errorf("PostgreSQL 18 or newer is required by the Helmr schema baseline; server_version_num=%d", serverVersion)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.organizations') IS NOT NULL`).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		if err := verifySchemaVersion(ctx, pool); err != nil {
			return false, err
		}
		return false, nil
	}
	migrations, err := migrationPaths()
	if err != nil {
		return false, err
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
	}
	version, err := schema.CurrentVersion()
	if err != nil {
		return false, err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (version BIGINT NOT NULL PRIMARY KEY, dirty BOOLEAN NOT NULL)`); err != nil {
		return false, fmt.Errorf("create migration version table: %w", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES ($1, FALSE)`, version); err != nil {
		return false, fmt.Errorf("record migration version: %w", err)
	}
	return true, nil
}

func verifySchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := schema.CurrentVersion()
	if err != nil {
		return err
	}
	var version int64
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty); err != nil {
		return fmt.Errorf("existing database is missing schema_migrations: %w", err)
	}
	if dirty || version != int64(want) {
		return fmt.Errorf(
			"database schema version %d (dirty=%v) does not match current schema version %d; reset an owned dev database with make dev-reset or migrate the external database",
			version,
			dirty,
			want,
		)
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

func devLogin(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, queries *db.Queries, cfg devConfig) {
	ctx := r.Context()
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
