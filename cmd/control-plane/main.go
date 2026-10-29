package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/platformhttp"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	clickhouseschema "github.com/helmrdotdev/helmr/internal/clickhouse/schema"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/controlplane"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	dbschema "github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/email"
	emailresend "github.com/helmrdotdev/helmr/internal/email/resend"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

type backgroundWorkflow struct {
	name string
	run  func(context.Context) error
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.String())
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "operator":
			if err := runOperatorCommand(context.Background(), os.Args[2:]); err != nil {
				log.Error("operator enrollment failed", "error", err)
				os.Exit(1)
			}
			return
		case "check-config":
			if len(os.Args) != 2 {
				log.Error("usage: control-plane check-config")
				os.Exit(1)
			}
			if _, err := config.LoadControlPlane(); err != nil {
				log.Error("invalid environment configuration", "error", err)
				os.Exit(1)
			}
			fmt.Println("Environment configuration is valid; dependencies and runtime files were not checked.")
			return
		case "audit":
			if err := runAuditCommand(context.Background(), os.Stdout, os.Args[2:]); err != nil {
				log.Error("audit maintenance failed", "error", err)
				os.Exit(1)
			}
			return
		case "migrate":
			if err := runMigrate(log, os.Args[2:]); err != nil {
				log.Error("migrate database", "error", err)
				os.Exit(1)
			}
			return
		case "clickhouse-bootstrap":
			if err := runClickHouseBootstrap(context.Background(), os.Args[2:]); err != nil {
				log.Error("bootstrap ClickHouse accounts", "error", err)
				os.Exit(1)
			}
			return
		case "database-bootstrap":
			if err := runDatabaseBootstrap(context.Background(), os.Args[2:]); err != nil {
				log.Error("bootstrap database", "error", err)
				os.Exit(1)
			}
			return
		case "release":
			if err := runReleaseCommand(context.Background(), os.Args[2:]); !errors.Is(err, nil) {
				log.Error("publish platform release", "error", err)
				os.Exit(1)
			}
			return
		case "worker-group":
			if err := runWorkerGroupStatusCommand(context.Background(), os.Stdout, os.Args[2:]); err != nil {
				log.Error("manage worker group state", "error", err)
				os.Exit(1)
			}
			return
		case "worker-host":
			if err := runWorkerHostStatusCommand(context.Background(), os.Stdout, os.Args[2:]); err != nil {
				log.Error("manage worker instance state", "error", err)
				os.Exit(1)
			}
			return
		default:
			log.Error("unknown command", "command", os.Args[1])
			os.Exit(1)
		}
	}
	if err := runControlPlane(context.Background(), log); err != nil {
		log.Error("Control Plane stopped", "error", err)
		os.Exit(1)
	}
}

func runControlPlane(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadControlPlane()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	external, err := platformhttp.External(cfg.OutboundProxyURL)
	if err != nil {
		return err
	}
	defer external.CloseIdleConnections()
	clickHouseConfig := clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	}
	if err := clickhouse.ValidateConfig(clickHouseConfig); err != nil {
		return fmt.Errorf("validate clickhouse config: %w", err)
	}
	if err := cass3.ValidateDistinctS3Stores(cfg.CASURI, cfg.PlatformStoreURI); err != nil {
		return fmt.Errorf("validate platform artifact store: %w", err)
	}
	appOrigin, err := url.Parse(cfg.AppOrigin)
	if err != nil {
		return fmt.Errorf("parse application origin: %w", err)
	}
	runtimeRaw, err := os.ReadFile(cfg.DeploymentRuntimeDescriptorPath)
	if err != nil {
		return fmt.Errorf("read deployment Runtime descriptor: %w", err)
	}
	runtimeDescriptor, err := artifact.ParseRuntimeDescriptor(runtimeRaw)
	if err != nil {
		return fmt.Errorf("parse deployment Runtime descriptor: %w", err)
	}
	bundleAdmission := bundle.Admission{Runtime: runtimeDescriptor}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	pool, err := dbpool.New(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := audit.ValidatePrivileges(ctx, pool, false); err != nil {
		return fmt.Errorf("validate audit database privileges: %w", err)
	}
	if err := audit.ObservePolicy(ctx, pool, cfg.AuditPolicy, audit.ControlPlane); err != nil {
		return fmt.Errorf("record audit retention configuration: %w", err)
	}

	diagnosticConfig := poolConfig.Copy()
	diagnosticConfig.MaxConns = cfg.DiagnosticAdmission.MaxConnections
	diagnosticConfig.MinConns = 0
	diagnosticPool, err := dbpool.New(ctx, diagnosticConfig)
	if err != nil {
		return fmt.Errorf("connect diagnostic database: %w", err)
	}
	defer diagnosticPool.Close()
	log.Info("control plane database connection budget", "lifecycle", poolConfig.MaxConns, "diagnostic", diagnosticConfig.MaxConns, "notifications", 1)
	queries := db.New(pool)
	if cfg.Bootstrap.Enabled {
		if err := workergroup.Bootstrap(ctx, pool, workergroup.BootstrapConfig{AuditPolicy: cfg.AuditPolicy,
			RegionID: cfg.Bootstrap.RegionID, RegionDisplayName: cfg.Bootstrap.RegionDisplayName,
			RegionLocation: cfg.Bootstrap.RegionLocation, GroupName: cfg.Bootstrap.WorkerGroupName,
			EnrollmentToken: cfg.Bootstrap.WorkerToken,
		}); err != nil {
			return fmt.Errorf("bootstrap platform: %w", err)
		}
	}
	clickHouseClient, err := clickhouse.New(clickHouseConfig)
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
	telemetryReader := clickhouse.NewReader(clickHouseClient)
	mailer := configuredEmailSender(log, cfg, external)
	shutdownTimeout := 10 * time.Second
	emailCodeDelivery := controlplane.NewEmailCodeDelivery(log, shutdownTimeout)
	secretStore, err := secret.New(queries, pool, cfg.EncryptionKey, cfg.AuditPolicy)
	if err != nil {
		return fmt.Errorf("configure secret store: %w", err)
	}
	casStore, err := cass3.New(ctx, cfg.CASURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	platformStore, err := cass3.NewImmutable(ctx, cfg.PlatformStoreURI)
	if err != nil {
		return fmt.Errorf("configure platform artifact store: %w", err)
	}
	var authProvider controlplane.AuthProvider
	if cfg.GitHubOAuthClientID != "" && cfg.GitHubOAuthClientSecret != "" {
		authProvider = controlplane.NewGitHubOAuthProvider(log, cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret, appOrigin, external)
	}
	computerKeys, err := configuredComputerKeys(ctx, cfg)
	if err != nil {
		return err
	}
	allocator, err := dispatch.NewAllocator(pool, cfg.AuditPolicy, cfg.ComputerFencingKey, secretStore)
	if err != nil {
		return fmt.Errorf("configure allocation owner: %w", err)
	}
	var slackConfig *controlplane.SlackConfig
	if cfg.SlackTransport != "disabled" {
		credentials, err := slack.NewCredentialStore(pool, cfg.EncryptionKey, slack.Transport(cfg.SlackTransport), cfg.AuditPolicy)
		if err != nil {
			return fmt.Errorf("configure Slack credentials: %w", err)
		}
		controlKey, err := slack.ControlKey(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("configure Slack controls: %w", err)
		}
		slackConfig = &controlplane.SlackConfig{ControlKey: controlKey, Credentials: credentials, Transport: external, Client: slack.NewWebClient(credentials, external)}
	}
	changes := dbnotify.New()
	allocationWake, stopAllocationWake := changes.Subscribe("dispatch")
	defer stopAllocationWake()
	handler, err := controlplane.NewServer(controlplane.ServerConfig{AuditPolicy: cfg.AuditPolicy,
		NetworkInstallation:            networkpolicy.Installation{PrivateBusinessCIDRs: cfg.NetworkPrivateBusinessCIDRs, BlockedIPv4CIDRs: cfg.NetworkBlockedIPv4CIDRs},
		Changes:                        changes,
		Slack:                          slackConfig,
		Allocator:                      allocator,
		DiagnosticDB:                   diagnosticPool,
		DiagnosticBounds:               cfg.DiagnosticAdmission.Bounds,
		DeploymentUploadLimits:         controlplane.DeploymentUploadLimits(cfg.DeploymentUploadLimits),
		EnvironmentExecutionLimits:     org.ExecutionLimits(cfg.EnvironmentExecutionLimits),
		ComputerKeys:                   computerKeys,
		Log:                            log,
		DeploymentMode:                 cfg.DeploymentMode,
		OrganizationCreationAdminOnly:  cfg.OrganizationCreationAdminOnly,
		DB:                             queries,
		TX:                             pool,
		ReadinessDB:                    pool,
		Auth:                           identity.NewAPIKeyAuthenticator(pool),
		CAS:                            casStore,
		BundleAdmission:                bundleAdmission,
		PlatformStore:                  platformStore,
		Secrets:                        secretStore,
		SecretDelivery:                 secretStore,
		SecretProxy:                    secretStore,
		TelemetryReader:                telemetryReader,
		Mailer:                         mailer,
		EmailCodeDelivery:              emailCodeDelivery,
		AuthProvider:                   authProvider,
		WorkerHostCredentialSigningKey: cfg.WorkerHostCredentialSigningKey,
		CapacityToken:                  cfg.CapacityToken,
		AuthKey:                        cfg.AuthKey,
		AppOrigin:                      appOrigin,
	})
	if err != nil {
		return fmt.Errorf("configure control plane server: %w", err)
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}
	casReclaimer, err := computer.NewCASReclaimer(pool, casStore, log)
	if err != nil {
		return fmt.Errorf("configure CAS reclamation: %w", err)
	}

	workflows := []backgroundWorkflow{
		{name: "database notifications", run: func(ctx context.Context) error { return changes.Run(ctx, pool.Config().ConnConfig, log) }},
		{name: "CAS reclamation", run: casReclaimer.Run},
		{name: "Computer disk retention", run: func(ctx context.Context) error { return computer.RunDiskRetention(ctx, pool, log) }},
		{name: "allocation", run: func(ctx context.Context) error { return allocator.Run(ctx, log, allocationWake) }},
		{name: "commands", run: func(ctx context.Context) error { return command.ReconcileCommands(ctx, cfg.AuditPolicy, pool, log) }},
		{name: "Session history retention", run: func(ctx context.Context) error { return slack.RunHistoryRetention(ctx, pool, log) }},
		{name: "Session lifecycle", run: func(ctx context.Context) error { return session.RunSessionLifecycle(ctx, cfg.AuditPolicy, pool, log) }},
		{name: "Computer retention", run: func(ctx context.Context) error { return computer.RunRetention(ctx, cfg.AuditPolicy, pool, log) }},
		{name: "Computer execution admissions", run: func(ctx context.Context) error {
			return dispatch.RunExecutionAdmissions(ctx, cfg.AuditPolicy, pool, log)
		}},
		{name: "Computer execution controls", run: func(ctx context.Context) error { return computer.RunExecutionControls(ctx, cfg.AuditPolicy, pool, log) }},
		{name: "Computer lifecycle", run: func(ctx context.Context) error { return dispatch.RunLeaseLifecycle(ctx, cfg.AuditPolicy, pool, log) }},
		{name: "preparation lifecycle", run: func(ctx context.Context) error {
			return dispatch.RunImageBuildLifecycle(ctx, cfg.AuditPolicy, pool, log)
		}},
		{name: "Deployment event retention", run: func(ctx context.Context) error { return deployment.RunEventRetention(ctx, pool, log) }},
		{name: "operation receipt retention", run: func(ctx context.Context) error {
			return idempotency.CollectReceipts(ctx, queries, log)
		}},
		{name: "exec result retention", run: func(ctx context.Context) error {
			return command.CollectResults(ctx, queries, log)
		}},
		{name: "email code delivery", run: emailCodeDelivery.Run},
		{name: "account security notifications", run: func(ctx context.Context) error { return controlplane.RunSecurityNotifications(ctx, pool, mailer, log) }},
	}
	return serveControlPlane(ctx, log, server, workflows, shutdownTimeout)
}

func serveControlPlane(
	ctx context.Context,
	log *slog.Logger,
	server *http.Server,
	workflows []backgroundWorkflow,
	shutdownTimeout time.Duration,
) error {
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	server.BaseContext = func(_ net.Listener) context.Context {
		return serverCtx
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	workflowErr := make(chan error, len(workflows))
	var workflowWG sync.WaitGroup
	for _, workflow := range workflows {
		workflowWG.Go(func() {
			err := workflow.run(backgroundCtx)
			if err == nil {
				err = errors.New("stopped unexpectedly")
			}
			workflowErr <- fmt.Errorf("%s: %w", workflow.name, err)
		})
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(listener)
	}()
	log.Info("Helmr Control Plane listening", "addr", listener.Addr().String())
	var runErr error
	serverStopped := false
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		serverStopped = true
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			runErr = errors.New("HTTP server stopped unexpectedly")
		} else {
			runErr = fmt.Errorf("serve: %w", err)
		}
	case err := <-workflowErr:
		runErr = err
	}
	cancelBackground()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancelShutdown()
	if shutdownErr != nil {
		cancelServer()
		runErr = errors.Join(runErr, fmt.Errorf("shutdown server: %w", shutdownErr))
		if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("close server: %w", closeErr))
		}
	} else {
		cancelServer()
	}
	if !serverStopped {
		if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("serve: %w", err))
		}
	}
	workflowWG.Wait()
	close(workflowErr)
	for err := range workflowErr {
		if !errors.Is(err, context.Canceled) {
			runErr = errors.Join(runErr, err)
		}
	}
	return runErr
}

func configuredEmailSender(log *slog.Logger, cfg config.ControlPlane, transport http.RoundTripper) email.Sender {
	switch cfg.EmailProvider {
	case config.EmailProviderSMTP:
		return email.NewSMTPSender(cfg.SMTPAddr, cfg.SMTPUsername, cfg.SMTPPassword, cfg.EmailFrom)
	case config.EmailProviderResend:
		return emailresend.New(cfg.ResendAPIKey, cfg.EmailFrom, transport)
	case config.EmailProviderLog:
		return email.LogSender{Log: log}
	default:
		return email.Unconfigured{}
	}
}

func runMigrate(log *slog.Logger, args []string) error {
	if len(args) != 1 || args[0] != "up" {
		return errors.New("usage: control-plane migrate up")
	}
	migrationURL := os.Getenv("DATABASE_MIGRATION_URL")
	if migrationURL == "" {
		return errors.New("DATABASE_MIGRATION_URL is required")
	}
	runtimeConfig, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil || os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required")
	}
	maintenanceConfig, err := pgxpool.ParseConfig(os.Getenv("DATABASE_AUDIT_MAINTENANCE_URL"))
	if err != nil || os.Getenv("DATABASE_AUDIT_MAINTENANCE_URL") == "" {
		return errors.New("DATABASE_AUDIT_MAINTENANCE_URL is required")
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
	if err := dbschema.Up(ctx, migrationURL); err != nil {
		return err
	}
	if err := dbschema.ConfigurePrivileges(ctx, migrationURL, runtimeConfig.ConnConfig.User, maintenanceConfig.ConnConfig.User); err != nil {
		return err
	}
	log.Info("database migrations are up to date")
	return nil
}

// Provider selection is explicit; a managed provider failure never selects local
// wrapping material. The key ARN fixes the regional KMS endpoint.
func configuredComputerKeys(ctx context.Context, cfg config.ControlPlane) (computer.DataKeyWrapper, error) {
	switch cfg.DeploymentMode {
	case config.DeploymentModeSelfHosted:
		return computerkey.NewLocal(cfg.ComputerWrappingKeyID, cfg.ComputerWrappingKey)
	case config.DeploymentModeManagedCloud:
		key, err := arn.Parse(cfg.ComputerKMSKeyARN)
		if err != nil || key.Service != "kms" || key.Region == "" {
			return nil, errors.New("invalid computer KMS key ARN")
		}
		provider, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(key.Region), awsconfig.WithHTTPClient(&http.Client{Transport: platformhttp.Direct()}))
		if err != nil {
			return nil, errors.New("load computer KMS configuration")
		}
		return computerkey.NewKMS(kms.NewFromConfig(provider), cfg.ComputerKMSKeyARN)
	default:
		return nil, errors.New("computer key provider requires a deployment mode")
	}
}
