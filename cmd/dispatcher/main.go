package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/platformhttp"

	"github.com/helmrdotdev/helmr/internal/clickhouse"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/slack"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lifecycle and event export share this pool; diagnostics have a separate budget.
const baseMaxConns = int32(12)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.String())
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		if os.Args[1] != "check-config" || len(os.Args) != 2 {
			log.Error("usage: dispatcher [check-config|--version]")
			os.Exit(1)
		}
		if _, err := config.LoadDispatcher(); err != nil {
			log.Error("invalid environment configuration", "error", err)
			os.Exit(1)
		}
		fmt.Println("Environment configuration is valid; dependencies and runtime files were not checked.")
		return
	}
	if err := runDispatcher(context.Background(), log); err != nil {
		log.Error("dispatcher stopped", "error", err)
		os.Exit(1)
	}
}

func runDispatcher(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadDispatcher()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	external, err := platformhttp.External(cfg.OutboundProxyURL)
	if err != nil {
		return err
	}
	defer external.CloseIdleConnections()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := newDispatchPool(ctx, cfg.DatabaseURL, baseMaxConns)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := audit.ValidatePrivileges(ctx, pool, false); err != nil {
		return fmt.Errorf("validate audit database privileges: %w", err)
	}
	if err := audit.ObservePolicy(ctx, pool, cfg.AuditPolicy, "dispatcher"); err != nil {
		return fmt.Errorf("record audit retention configuration: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	diagnosticPool, err := newDispatchPool(ctx, cfg.DatabaseURL, cfg.DiagnosticExporter.MaxConnections)
	if err != nil {
		return fmt.Errorf("connect diagnostic database: %w", err)
	}
	defer diagnosticPool.Close()
	log.Info("dispatcher database connection budget", "lifecycle", baseMaxConns, "diagnostic", cfg.DiagnosticExporter.MaxConnections)
	queries := db.New(pool)
	clickHouseClient, err := clickhouse.New(clickhouse.Config{
		URL:      cfg.ClickHouseURL,
		User:     cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return fmt.Errorf("configure clickhouse: %w", err)
	}
	defer clickHouseClient.Close()
	staleHostFencer, err := workergroup.NewStaleHostFencer(pool, cfg.ControlPlaneURL, workergroup.WithStaleHostFenceLogger(log))
	if err != nil {
		return fmt.Errorf("configure stale worker fencer: %w", err)
	}
	secretStore, err := secret.New(queries, pool, cfg.EncryptionKey, cfg.AuditPolicy)
	if err != nil {
		return fmt.Errorf("configure scheduled Computer trust: %w", err)
	}
	diagnosticIngester, err := telemetry.NewDiagnosticIngester(diagnosticPool, clickhouse.NewWriter(clickHouseClient), cfg.DiagnosticExporter.Ingest, log)
	if err != nil {
		return fmt.Errorf("configure diagnostic exporter: %w", err)
	}

	changes := dbnotify.New()
	runners := []dispatcherRunner{
		{name: "database notifications", run: func(ctx context.Context) error { return changes.Run(ctx, pool.Config().ConnConfig, log) }},
		{name: "stale host fencer", run: staleHostFencer.Run},
		{name: "Agent triggers", run: func(ctx context.Context) error {
			return agent.RunTriggers(ctx, cfg.AuditPolicy, pool, secretStore, log)
		}},
		{name: "Deployment event retention", run: func(ctx context.Context) error { return deployment.RunEventRetention(ctx, pool, log) }},
		{name: "diagnostic exporter", run: diagnosticIngester.Run},
	}
	if cfg.SlackTransport != "disabled" {
		credentials, err := slack.NewCredentialStore(pool, cfg.EncryptionKey, slack.Transport(cfg.SlackTransport), cfg.AuditPolicy)
		if err != nil {
			return fmt.Errorf("configure Slack credentials: %w", err)
		}
		client := slack.NewWebClient(credentials, external)
		appOrigin, err := url.Parse(cfg.AppOrigin)
		if err != nil {
			return fmt.Errorf("configure Slack Console URL: %w", err)
		}
		controlKey, err := slack.ControlKey(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("configure Slack controls: %w", err)
		}
		if cfg.SlackTransport == "socket" {
			sockets, err := slack.NewSocketSupervisor(slack.SocketConfig{Credentials: credentials, Client: client, AppOrigin: appOrigin, ControlKey: controlKey, Transport: external, Log: log})
			if err != nil {
				return err
			}
			runners = append(runners, dispatcherRunner{name: "Slack sockets", run: sockets.Run})
		}
		projection := slack.ProjectionConfig{AppOrigin: appOrigin, ControlKey: controlKey}
		runners = append(runners,
			dispatcherRunner{name: "Slack requests", run: func(ctx context.Context) error {
				return slack.RunRequests(ctx, cfg.AuditPolicy, pool, secretStore, projection, client, log, changes)
			}},
			dispatcherRunner{name: "Slack projection", run: func(ctx context.Context) error { return slack.RunProjection(ctx, pool, projection, log, changes) }},
			dispatcherRunner{name: "Slack credentials", run: func(ctx context.Context) error { return credentials.RunCredentials(ctx, external, log) }},
			dispatcherRunner{name: "Slack delivery", run: func(ctx context.Context) error {
				return slack.RunDelivery(ctx, pool, cfg.AuditPolicy, client, log, changes)
			}},
		)
	}
	log.Info("Helmr dispatcher running")
	return superviseRunners(ctx, runners)
}

type dispatcherRunner struct {
	name string
	run  func(context.Context) error
}

var errRunnerStoppedUnexpectedly = errors.New("stopped unexpectedly")

// superviseRunners runs every runner until ctx is cancelled or any runner
// exits. A runner that returns nil or context.Canceled while the supervisor
// has not cancelled it is an unexpected exit. The first exit cancels the peers,
// every runner is joined, and all failures are returned together.
func superviseRunners(ctx context.Context, runners []dispatcherRunner) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(runners))
	var wg sync.WaitGroup
	for _, runner := range runners {
		wg.Go(func() {
			err := runner.run(runCtx)
			stopping := runCtx.Err() != nil
			switch {
			case stopping && (err == nil || errors.Is(err, context.Canceled)):
				errc <- nil
			case err == nil:
				errc <- fmt.Errorf("%s: %w", runner.name, errRunnerStoppedUnexpectedly)
			case errors.Is(err, context.Canceled):
				errc <- fmt.Errorf("%s: %w: returned %v", runner.name, errRunnerStoppedUnexpectedly, err)
			default:
				errc <- fmt.Errorf("%s: %w", runner.name, err)
			}
		})
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	cancel()
	wg.Wait()
	close(errc)
	for err := range errc {
		runErr = errors.Join(runErr, err)
	}
	return runErr
}

func newDispatchPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	poolConfig.MaxConns = maxConns
	pool, err := dbpool.New(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
