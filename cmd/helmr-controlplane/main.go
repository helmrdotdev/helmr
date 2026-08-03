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
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsecr "github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/helmrdotdev/helmr/internal/auth"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	clickhouseschema "github.com/helmrdotdev/helmr/internal/clickhouse/schema"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/controlplane"
	"github.com/helmrdotdev/helmr/internal/db"
	dbschema "github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/email"
	emailresend "github.com/helmrdotdev/helmr/internal/email/resend"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/eventstream"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	imagecacheecr "github.com/helmrdotdev/helmr/internal/imagecache/ecr"
	"github.com/helmrdotdev/helmr/internal/platformlock"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var loadControlPlaneBuildPolicy = func(path string) (*deployment.BuildPolicy, error) {
	policy, err := deployment.LoadBuildPolicy(path)
	if err != nil {
		return nil, fmt.Errorf("load build policy: %w", err)
	}
	return policy, nil
}

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
		case "release":
			if err := runReleaseCommand(context.Background(), os.Args[2:]); err != nil {
				log.Error("install release", "error", err)
				os.Exit(1)
			}
			return
		case "worker-group":
			if err := runWorkerGroupStateCommand(context.Background(), os.Stdout, os.Args[2:]); err != nil {
				log.Error("manage worker group state", "error", err)
				os.Exit(1)
			}
			return
		case "worker-instance":
			if err := runWorkerInstanceStateCommand(context.Background(), os.Stdout, os.Args[2:]); err != nil {
				log.Error("manage worker instance state", "error", err)
				os.Exit(1)
			}
			return
		default:
			log.Error("unknown command", "command", os.Args[1])
			os.Exit(1)
		}
	}
	if err := run(context.Background(), log); err != nil {
		log.Error("Control Plane stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadControlPlane()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	buildPolicy, err := loadControlPlaneBuildPolicy(cfg.BuildPolicyPath)
	if err != nil {
		return err
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
	bootstrapCfg, err := config.LoadRegionBootstrap()
	if err != nil {
		return fmt.Errorf("load region bootstrap config: %w", err)
	}
	groups, err := workergroup.DecodeConfig(cfg.WorkerGroupsJSON)
	if err != nil {
		return fmt.Errorf("decode HELMR_WORKER_GROUPS: %w", err)
	}
	desiredGroups := make([]workergroup.Desired, 0, len(groups))
	enrollmentSecrets := make([]enrollment.GroupSecret, 0, len(groups))
	for _, configuredGroup := range groups {
		desired, groupSecret, err := configuredGroup.Prepare(os.LookupEnv)
		if err != nil {
			return fmt.Errorf("prepare worker group %q: %w", configuredGroup.ID, err)
		}
		desiredGroups = append(desiredGroups, desired)
		enrollmentSecrets = append(enrollmentSecrets, groupSecret)
	}
	workerEnrollment, err := enrollment.NewVerifier(enrollmentSecrets)
	if err != nil {
		return fmt.Errorf("configure worker enrollment: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin worker group reconciliation: %w", err)
	}
	defer tx.Rollback(ctx)
	txQueries := db.New(tx)
	if err := region.Ensure(ctx, txQueries, region.BootstrapConfig{
		RegionID:          bootstrapCfg.RegionID,
		DefaultRegionID:   bootstrapCfg.DefaultRegionID,
		Provider:          bootstrapCfg.Provider,
		ProviderRegion:    bootstrapCfg.ProviderRegion,
		RegionDisplayName: bootstrapCfg.RegionDisplayName,
	}); err != nil {
		return fmt.Errorf("bootstrap region: %w", err)
	}
	if err := workergroup.Reconcile(ctx, txQueries, bootstrapCfg.RegionID, desiredGroups); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit worker group reconciliation: %w", err)
	}
	clickHouseClient, err := clickhouse.New(clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
	telemetryReader := clickhouse.NewReader(clickHouseClient)
	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("parse public URL: %w", err)
	}
	mailer := configuredEmailSender(log, cfg)
	eventStream, err := eventstream.New(log, queries, redisClient, eventstream.Config{
		TelemetryReader: telemetryReader,
	})
	if err != nil {
		return fmt.Errorf("configure event stream: %w", err)
	}
	go func() {
		if err := eventStream.RunPublisher(backgroundCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("live telemetry publisher stopped", "error", err)
			cancelServer()
		}
	}()
	secretStore, err := secret.New(queries, pool, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	workspaceFencingKey, err := workspace.NewFencingKey(cfg.WorkspaceFencingKey)
	if err != nil {
		return fmt.Errorf("configure Workspace fencing key: %w", err)
	}
	tokenCredentialKey, err := auth.NewCredentialKey(cfg.TokenCredentialKey)
	if err != nil {
		return fmt.Errorf("configure Token credential key: %w", err)
	}
	casStore, err := cass3.New(ctx, cfg.CASURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	if err := cass3.ValidateDistinctS3Stores(cfg.CASURI, cfg.PlatformStoreURI); err != nil {
		return fmt.Errorf("validate Platform Artifact store: %w", err)
	}
	platformStore, err := cass3.NewImmutable(ctx, cfg.PlatformStoreURI)
	if err != nil {
		return fmt.Errorf("configure Platform Artifact store: %w", err)
	}
	platformArtifactLocks, err := platformlock.New(pool)
	if err != nil {
		return fmt.Errorf("configure Platform Artifact locks: %w", err)
	}
	var authProvider controlplane.AuthProvider
	if cfg.GitHubOAuthClientID != "" && cfg.GitHubOAuthClientSecret != "" {
		authProvider = controlplane.NewGitHubOAuthProvider(cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret, publicURL)
	}
	var cacheRepositories imagecache.RepositoryProvisioner
	var cacheRetirer imagecache.RepositoryRetirer
	if cfg.ImageCache != nil {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("load Workspace image cache AWS configuration: %w", err)
		}
		provisioner, err := imagecacheecr.NewProvisioner(
			imagecacheecr.Config{
				RegistryAuthority:   cfg.ImageCache.RegistryAuthority,
				RepositoryPrefix:    cfg.ImageCache.RepositoryPrefix,
				CacheRoleARN:        cfg.ImageCache.CacheRoleARN,
				RepositoryARNPrefix: cfg.ImageCache.RepositoryARNPrefix,
			},
			awsecr.NewFromConfig(awsCfg),
		)
		if err != nil {
			return fmt.Errorf("configure Workspace image cache repositories: %w", err)
		}
		cacheRepositories = provisioner
		cacheRetirer = provisioner
		go controlplane.RunImageCacheRetirement(backgroundCtx, log, queries, cacheRetirer)
	}
	handler, err := controlplane.NewServer(controlplane.ServerConfig{
		Log:                   log,
		DeploymentMode:        cfg.DeploymentMode,
		DB:                    queries,
		TX:                    pool,
		ReadinessDB:           pool,
		Auth:                  controlplane.NewDBAuthenticator(queries),
		CAS:                   casStore,
		BuildPolicy:           buildPolicy,
		PlatformStore:         platformStore,
		PlatformArtifactLocks: platformArtifactLocks,
		Secrets:               secretStore,
		SecretDelivery:        secretStore,
		RegistryCredentials:   secretStore,
		CacheRepositories:     cacheRepositories,
		WorkspaceFencingKey:   workspaceFencingKey,
		TokenCredentialKey:    tokenCredentialKey,
		EventStream:           eventStream,
		TelemetryReader:       telemetryReader,
		Mailer:                mailer,
		AuthProvider:          authProvider,
		WorkerTokenSigningKey: cfg.WorkerTokenSigningKey,
		RunLeaseTTL:           cfg.RunLeaseTTL,
		RunFinalizationTTL:    cfg.RunFinalizationTTL,
		WorkerEnrollment:      workerEnrollment,
		OperatorToken:         cfg.OperatorToken,
		SetupToken:            cfg.SetupToken,
		AuthKey:               cfg.AuthKey,
		PublicURL:             publicURL,
		MagicLinkDebugURLs:    cfg.MagicLinkDebugURLs,
		BackgroundContext:     backgroundCtx,
	})
	if err != nil {
		return fmt.Errorf("configure Control Plane server: %w", err)
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
	log.Info("Helmr Control Plane listening", "addr", cfg.Addr)
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

func configuredEmailSender(log *slog.Logger, cfg config.ControlPlane) email.Sender {
	switch cfg.EmailProvider {
	case config.EmailProviderSMTP:
		return email.NewSMTPSender(cfg.SMTPAddr, cfg.SMTPUsername, cfg.SMTPPassword, cfg.EmailFrom)
	case config.EmailProviderResend:
		return emailresend.New(cfg.ResendAPIKey, cfg.EmailFrom)
	case config.EmailProviderLog:
		return email.LogSender{Log: log}
	default:
		return email.Unconfigured{}
	}
}

func runMigrate(log *slog.Logger, args []string) error {
	if len(args) != 1 || args[0] != "up" {
		return errors.New("usage: helmr-controlplane migrate up")
	}
	cfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	clickHouseCfg, err := config.LoadClickHouse()
	if err != nil {
		return fmt.Errorf("load clickhouse config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := clickhouseschema.Up(ctx, clickhouse.Config{
		URL:      clickHouseCfg.URL,
		User:     clickHouseCfg.User,
		Password: clickHouseCfg.Password,
	}); err != nil {
		return err
	}
	if err := dbschema.Up(ctx, cfg.URL); err != nil {
		return err
	}
	log.Info("database migrations are up to date")
	return nil
}
