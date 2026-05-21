package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatcher"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/server"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAddr                = ":8080"
	defaultPublicURL           = "http://127.0.0.1:3000"
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
	secretKey, err := secret.KeyFromBase64(cfg.secretEncryptionKey)
	if err != nil {
		log.Error("load secret encryption key", "error", err)
		os.Exit(1)
	}
	secretStore, err := secret.New(queries, secret.DefaultKeyID, secretKey)
	if err != nil {
		log.Error("configure secret store", "error", err)
		os.Exit(1)
	}
	sweeper, err := dispatcher.NewSweeper(queries, dispatcher.WithLogger(log))
	if err != nil {
		log.Error("configure sweeper", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := sweeper.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sweeper stopped", "error", err)
		}
	}()

	app := server.New(
		log,
		server.WithDeploymentMode(cfg.deploymentMode),
		server.WithDBTX(pool),
		server.WithGitHubResolver(devGitHubResolver{}),
		server.WithCAS(casStore),
		server.WithSecrets(secretStore),
		server.WithWorkerAuth(cfg.workerTokenSecret, 0),
		server.WithDefaultWorkerBootstrapToken(cfg.workerBootstrapToken),
		server.WithInitialSetupToken(cfg.setupToken),
		server.WithUserAuth(cfg.authSecret, cfg.publicURL),
	)
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
	addr                 string
	deploymentMode       string
	databaseURL          string
	casDir               string
	publicURL            string
	authSecret           string
	setupToken           string
	workerBootstrapToken string
	workerTokenSecret    string
	secretEncryptionKey  string
	resetDatabase        bool
}

func loadConfig() (devConfig, error) {
	cfg := devConfig{
		addr:                 env("HELMR_CONTROL_ADDR", defaultAddr),
		deploymentMode:       env("HELMR_DEPLOYMENT_MODE", "self-hosted"),
		databaseURL:          os.Getenv("HELMR_DATABASE_URL"),
		casDir:               env("HELMR_DEV_CAS_DIR", filepath.Join(os.TempDir(), "helmr-dev-cas")),
		publicURL:            env("HELMR_PUBLIC_URL", defaultPublicURL),
		authSecret:           env("HELMR_AUTH_SECRET", defaultAuthSecret),
		setupToken:           env("HELMR_SETUP_TOKEN", defaultSetupToken),
		workerBootstrapToken: strings.TrimSpace(os.Getenv("HELMR_WORKER_BOOTSTRAP_TOKEN")),
		workerTokenSecret:    env("HELMR_WORKER_TOKEN_SIGNING_KEY", defaultWorkerTokenSecret),
		secretEncryptionKey:  env("HELMR_SECRET_ENCRYPTION_KEY", defaultSecretEncryptionKey),
		resetDatabase:        envBool("HELMR_DEV_RESET_DATABASE"),
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
	migrations, err := filepath.Glob("internal/db/schema/migrations/*.up.sql")
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
`, ids.ToPG(userID)); err != nil {
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
		ID:        ids.ToPG(ids.New()),
		UserID:    ids.ToPG(userID),
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
	http.Redirect(w, r, "/runs", http.StatusFound)
}

type devGitHubResolver struct{}

func (devGitHubResolver) ResolveCommit(_ context.Context, installationID int64, githubRepositoryID int64, source api.GitHubSource) (ghapp.ResolvedSource, error) {
	normalized, err := ghapp.NormalizeSource(source)
	if err != nil {
		return ghapp.ResolvedSource{}, err
	}
	normalized.SHA = "0123456789abcdef0123456789abcdef01234567"
	return ghapp.ResolvedSource{Source: normalized, InstallationID: installationID, GitHubRepositoryID: githubRepositoryID}, nil
}

func (devGitHubResolver) CreateRepositoryToken(context.Context, int64, int64) (ghapp.InstallationToken, error) {
	return ghapp.InstallationToken{Token: "helmr-dev-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (devGitHubResolver) InstallURL() string {
	return "https://github.com/apps/helmr-dev/installations/new"
}

func (devGitHubResolver) VerifyUserInstallation(context.Context, string, int64) (ghapp.VerifiedInstallation, error) {
	return ghapp.VerifiedInstallation{}, errors.New("github oauth is not configured for the local dev control")
}

func mustUUID(value string) uuid.UUID {
	parsed, err := uuid.Parse(value)
	if err != nil {
		panic(err)
	}
	return parsed
}
