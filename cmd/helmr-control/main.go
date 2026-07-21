package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	clickhouseschema "github.com/helmrdotdev/helmr/internal/clickhouse/schema"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	dbschema "github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	dispatchredis "github.com/helmrdotdev/helmr/internal/dispatch/redis"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var loadControlBuildPolicy = func(path string) (*deployment.BuildPolicy, error) {
	catalog, err := deployment.LoadRuntimeCatalog()
	if err != nil {
		return nil, fmt.Errorf("authenticate managed runtime catalog: %w", err)
	}
	toolchains, err := deployment.LoadToolchainCatalog()
	if err != nil {
		return nil, fmt.Errorf("authenticate standard toolchain catalog: %w", err)
	}
	policy, err := deployment.LoadBuildPolicy(path, catalog, toolchains)
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
		case "secrets":
			if err := runSecretsCommand(log, os.Args[2:]); err != nil {
				log.Error("manage secrets", "error", err)
				os.Exit(1)
			}
			return
		case "lookup-hmac":
			if err := runLookupHMACCommand(log, os.Args[2:]); err != nil {
				log.Error("inspect lookup HMAC keys", "error", err)
				os.Exit(1)
			}
			return
		case "release":
			if err := runReleaseCommand(context.Background(), os.Args[2:]); err != nil {
				log.Error("install release", "error", err)
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
	buildPolicy, err := loadControlBuildPolicy(cfg.BuildPolicyPath)
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
	bootstrapCfg, err := config.LoadWorkerGroupBootstrap()
	if err != nil {
		return fmt.Errorf("load worker group bootstrap config: %w", err)
	}
	var groups []configuredWorkerGroup
	if err := json.Unmarshal([]byte(cfg.WorkerGroupsJSON), &groups); err != nil {
		return fmt.Errorf("decode HELMR_WORKER_GROUPS: %w", err)
	}
	awsGroups := make([]enrollment.AWSGroupBoundary, 0, len(groups))
	desiredGroups := make([]workergroup.Desired, 0, len(groups))
	for _, configuredGroup := range groups {
		boundary, desired, err := enrollment.PrepareAWSGroup(configuredGroup.awsWorkerGroup())
		if err != nil {
			return fmt.Errorf("prepare worker group %q: %w", configuredGroup.ID, err)
		}
		awsGroups = append(awsGroups, boundary)
		desiredGroups = append(desiredGroups, desired)
	}
	verifier, err := loadAWSWorkerEnrollmentVerifier(ctx, awsGroups)
	if err != nil {
		return err
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
	if err := workergroup.Reconcile(ctx, txQueries, cfg.RegionID, desiredGroups); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit worker group reconciliation: %w", err)
	}
	workerEnrollment := controlWorkerEnrollmentVerifier{verifier: verifier}
	clickHouseClient, err := clickhouse.New(clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
	telemetryReader := telemetry.NewHistoricalReader(clickHouseClient)
	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	redisClient := redis.NewClient(redisOptions)
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
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("parse public URL: %w", err)
	}
	mailer := configuredEmailSender(log, cfg)
	eventStream, err := control.NewEventStream(log, queries, redisClient, control.EventStreamConfig{
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
	keyring, err := secret.KeyringFromBase64(cfg.SecretEncryptionKey, cfg.SecretEncryptionKeyOld)
	if err != nil {
		return fmt.Errorf("load secret encryption key: %w", err)
	}
	hashes, err := keyedhash.FromBase64JSON(cfg.LookupHMACKeys)
	if err != nil {
		return fmt.Errorf("load lookup HMAC keys: %w", err)
	}
	secretStore, err := secret.New(ctx, queries, pool, keyring, hashes)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	scheduleRunCreator, err := control.NewScheduleRunCreator(log, pool, secretStore, runEnqueuer, eventStream)
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
	if err := cas.ValidateDistinctS3Stores(cfg.CASURI, cfg.RuntimeStoreURI); err != nil {
		return fmt.Errorf("validate managed runtime store: %w", err)
	}
	if err := cas.ValidateDistinctS3Stores(cfg.CASURI, cfg.ManagerStoreURI); err != nil {
		return fmt.Errorf("validate manager store: %w", err)
	}
	if err := cas.ValidateDistinctS3Stores(
		cfg.RuntimeStoreURI,
		cfg.ManagerStoreURI,
	); err != nil {
		return fmt.Errorf("validate manager and managed runtime stores: %w", err)
	}
	runtimeStore, err := cas.NewImmutableS3(ctx, cfg.RuntimeStoreURI)
	if err != nil {
		return fmt.Errorf("configure managed runtime store: %w", err)
	}
	managerStore, err := deployment.NewManagerS3(ctx, cfg.ManagerStoreURI)
	if err != nil {
		return fmt.Errorf("configure manager store: %w", err)
	}
	var authProvider control.AuthProvider
	if cfg.GitHubOAuthClientID != "" && cfg.GitHubOAuthClientSecret != "" {
		authProvider = control.NewGitHubOAuthProvider(cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret, publicURL)
	}
	handler, err := control.NewServer(control.ServerConfig{
		Log:                log,
		DeploymentMode:     cfg.DeploymentMode,
		WorkerGroupID:      cfg.WorkerGroupID,
		RegionID:           cfg.RegionID,
		DefaultRegionID:    cfg.DefaultRegionID,
		DB:                 queries,
		TX:                 pool,
		ReadinessDB:        pool,
		Auth:               auth.NewDBAuthenticator(queries),
		CAS:                casStore,
		BuildPolicy:        buildPolicy,
		RuntimeStore:       runtimeStore,
		ManagerStore:       managerStore,
		Secrets:            secretStore,
		RunEnqueuer:        runEnqueuer,
		DispatchQueue:      dispatchQueue,
		ScheduleEngine:     scheduleEngine,
		EventStream:        eventStream,
		TelemetryReader:    telemetryReader,
		Mailer:             mailer,
		AuthProvider:       authProvider,
		WorkerTokenSecret:  []byte(cfg.WorkerTokenSigningKey),
		WorkerEnrollment:   workerEnrollment,
		SetupToken:         cfg.SetupToken,
		AuthSecret:         []byte(cfg.AuthSecret),
		PublicURL:          publicURL,
		MagicLinkDebugURLs: cfg.MagicLinkDebugURLs,
		BackgroundContext:  backgroundCtx,
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

var loadAWSWorkerEnrollmentVerifier = func(ctx context.Context, groups []enrollment.AWSGroupBoundary) (*enrollment.AWSVerifier, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS worker enrollment configuration: %w", err)
	}
	verifier, err := enrollment.NewAWSVerifier(awsCfg, groups)
	if err != nil {
		return nil, fmt.Errorf("configure AWS worker enrollment: %w", err)
	}
	return verifier, nil
}

type controlWorkerEnrollmentVerifier struct {
	verifier *enrollment.AWSVerifier
}

func (v controlWorkerEnrollmentVerifier) VerifyWorkerEnrollment(ctx context.Context, request api.WorkerEnrollmentRequest) (control.VerifiedWorkerEnrollment, error) {
	verified, err := v.verifier.VerifyWorkerEnrollment(ctx, request)
	if err != nil {
		return control.VerifiedWorkerEnrollment{}, err
	}
	return control.VerifiedWorkerEnrollment{
		WorkerGroupID: verified.WorkerGroupID, ResourceID: verified.ResourceID,
		AllowsRun: verified.AllowsRun, AllowsBuild: verified.AllowsBuild,
		ProtocolVersion:             verified.ProtocolVersion,
		EnrollmentPolicyFingerprint: verified.EnrollmentPolicyFingerprint,
		AttestationFingerprint:      verified.AttestationFingerprint,
	}, nil
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

func runSecretsCommand(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return secretCommandUsage()
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
	hashes, err := keyedhash.FromBase64JSON(os.Getenv("HELMR_LOOKUP_HMAC_KEYS"))
	if err != nil {
		return fmt.Errorf("load lookup HMAC keys: %w", err)
	}
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
	store, err := secret.New(ctx, queries, pool, keyring, hashes)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	switch args[0] {
	case "encryption-key-usage":
		if len(args) != 1 {
			return errors.New("usage: helmr-control secrets encryption-key-usage")
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
		limit, err := parseBatchLimit(args[1:], "reencrypt")
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
	case "reauthenticate":
		fromVersion, limit, err := parseReauthenticateArgs(args[1:])
		if err != nil {
			return err
		}
		result, err := store.ReauthenticateBatch(ctx, fromVersion, limit)
		if err != nil {
			return err
		}
		remaining, err := store.CountByAuthenticatorKeyVersion(ctx, fromVersion)
		if err != nil {
			return err
		}
		log.Info("secret re-authentication batch complete", "scanned", result.Scanned, "reauthenticated", result.Reauthenticated, "skipped", result.Skipped, "failed", result.Failed, "remaining_old_key_count", remaining)
		if result.Failed > 0 {
			return fmt.Errorf("%d secret versions could not be authenticated", result.Failed)
		}
		return nil
	default:
		return secretCommandUsage()
	}
}

func secretCommandUsage() error {
	return errors.New("usage: helmr-control secrets encryption-key-usage|reencrypt [--limit N]|reauthenticate --from-version N [--limit N]")
}

type lookupHMACUsage struct {
	claimCount         int64
	secretVersionCount int64
	current            bool
	active             bool
	configured         bool
	fingerprintMatches bool
}

func runLookupHMACCommand(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return lookupHMACCommandUsage()
	}
	cfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	hashes, err := keyedhash.FromBase64JSON(os.Getenv("HELMR_LOOKUP_HMAC_KEYS"))
	if err != nil {
		return fmt.Errorf("load lookup HMAC keys: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, cfg.URL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	queries := db.New(pool)
	authority := keyedhash.NewAuthority(hashes)
	switch args[0] {
	case "activate":
		version, err := parseLookupHMACVersion(args[1:], "activate")
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin lookup HMAC activation: %w", err)
		}
		defer tx.Rollback(ctx)
		created, err := authority.Activate(ctx, tx, version)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit lookup HMAC activation: %w", err)
		}
		log.Info("lookup HMAC version activated", "version", created.Version)
		return nil
	case "retire":
		version, err := parseLookupHMACVersion(args[1:], "retire")
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin lookup HMAC retirement: %w", err)
		}
		defer tx.Rollback(ctx)
		retired, err := authority.Retire(ctx, tx, version)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit lookup HMAC retirement: %w", err)
		}
		log.Info("lookup HMAC version retired", "version", retired.Version)
		return nil
	case "collect-claims":
		limit, err := parseLookupHMACClaimLimit(args[1:])
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin idempotency claim collection: %w", err)
		}
		defer tx.Rollback(ctx)
		txQueries := db.New(tx)
		retired, err := txQueries.RetireExpiredIdempotencyClaims(ctx, limit)
		if err != nil {
			return fmt.Errorf("retire expired idempotency claims: %w", err)
		}
		collected, err := txQueries.CollectRetiredIdempotencyClaims(ctx, limit)
		if err != nil {
			return fmt.Errorf("collect retired idempotency claims: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit idempotency claim collection: %w", err)
		}
		log.Info("idempotency claim collection complete", "retired", len(retired), "collected", len(collected))
		return nil
	case "key-usage":
		if len(args) != 1 {
			return lookupHMACCommandUsage()
		}
	default:
		return lookupHMACCommandUsage()
	}
	versions, err := queries.ListLookupHMACVersions(ctx)
	if err != nil {
		return err
	}
	claimUsage, err := queries.ListIdempotencyHashKeyUsage(ctx)
	if err != nil {
		return err
	}
	secretUsage, err := queries.ListSecretAuthenticatorKeyUsage(ctx)
	if err != nil {
		return err
	}
	usage := make(map[int32]lookupHMACUsage, len(versions))
	for _, row := range versions {
		value := lookupHMACUsage{
			current:    row.IsCurrent,
			active:     !row.RetiredAt.Valid,
			configured: hashes.Has(row.Version),
		}
		if value.configured {
			fingerprint, err := hashes.Fingerprint(row.Version)
			if err != nil {
				return err
			}
			value.fingerprintMatches = bytes.Equal(fingerprint[:], row.KeyFingerprint)
		}
		usage[row.Version] = value
	}
	for _, row := range claimUsage {
		value := usage[row.HashKeyVersion]
		value.claimCount = row.ClaimCount
		usage[row.HashKeyVersion] = value
	}
	for _, row := range secretUsage {
		value := usage[row.AuthenticatorKeyVersion]
		value.secretVersionCount = row.SecretCount
		usage[row.AuthenticatorKeyVersion] = value
	}
	ordered := make([]int, 0, len(usage))
	for version := range usage {
		ordered = append(ordered, int(version))
	}
	sort.Ints(ordered)
	for _, version := range ordered {
		value := usage[int32(version)]
		log.Info("lookup HMAC key usage",
			"version", version,
			"claim_count", value.claimCount,
			"secret_version_count", value.secretVersionCount,
			"current", value.current,
			"active", value.active,
			"configured", value.configured,
			"fingerprint_matches", value.fingerprintMatches,
		)
	}
	return nil
}

func parseLookupHMACVersion(args []string, command string) (int32, error) {
	if len(args) != 2 || args[0] != "--version" {
		return 0, lookupHMACCommandUsage()
	}
	version, err := strconv.ParseInt(args[1], 10, 32)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("%s --version must be a positive integer", command)
	}
	return int32(version), nil
}

func lookupHMACCommandUsage() error {
	return errors.New("usage: helmr-control lookup-hmac activate|retire --version N | collect-claims [--limit N] | key-usage")
}

func parseLookupHMACClaimLimit(args []string) (int32, error) {
	limit := int64(500)
	if len(args) == 0 {
		return int32(limit), nil
	}
	if len(args) != 2 || args[0] != "--limit" {
		return 0, lookupHMACCommandUsage()
	}
	parsed, err := strconv.ParseInt(args[1], 10, 32)
	if err != nil || parsed <= 0 {
		return 0, errors.New("collect-claims --limit must be a positive integer")
	}
	return int32(parsed), nil
}

func parseBatchLimit(args []string, command string) (int32, error) {
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
			return 0, fmt.Errorf("unknown secrets %s argument %q", command, args[i])
		}
	}
	if limit <= 0 {
		return 0, errors.New("--limit must be positive")
	}
	return int32(limit), nil
}

func parseReauthenticateArgs(args []string) (int32, int32, error) {
	var fromVersion int64
	limitArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] != "--from-version" {
			limitArgs = append(limitArgs, args[i])
			continue
		}
		if fromVersion != 0 {
			return 0, 0, errors.New("--from-version may only be specified once")
		}
		if i+1 >= len(args) {
			return 0, 0, errors.New("--from-version requires a value")
		}
		parsed, err := strconv.ParseInt(args[i+1], 10, 32)
		if err != nil || parsed <= 0 {
			return 0, 0, errors.New("--from-version must be a positive 32-bit integer")
		}
		fromVersion = parsed
		i++
	}
	if fromVersion == 0 {
		return 0, 0, errors.New("--from-version is required")
	}
	limit, err := parseBatchLimit(limitArgs, "reauthenticate")
	if err != nil {
		return 0, 0, err
	}
	return int32(fromVersion), limit, nil
}
