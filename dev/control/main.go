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
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatcher"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/server"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultAddr                = ":8080"
	defaultPublicURL           = "http://127.0.0.1:3000"
	defaultAuthSecret          = "helmr-dev-auth-secret-32-byte-value"
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
	bootstrap, err := control.Bootstrap(ctx, queries, true)
	if err != nil {
		log.Error("bootstrap control plane", "error", err)
		os.Exit(1)
	}
	if err := seedDemoData(ctx, pool, queries, casStore); err != nil {
		log.Error("seed demo data", "error", err)
		os.Exit(1)
	}
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
		server.WithDBTX(pool),
		server.WithGitHubResolver(devGitHubResolver{}),
		server.WithCAS(casStore),
		server.WithSecrets(secretStore),
		server.WithWorkerAuth(cfg.workerTokenSecret, 0),
		server.WithUserAuth(cfg.authSecret, cfg.publicURL),
		server.WithBootstrapOwnerEmail(cfg.bootstrapOwnerEmail),
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dev/login" {
			devLogin(ctx, w, r, queries, cfg)
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
	if bootstrap.SetupRequired {
		log.Info("owner bootstrap required", "bootstrap_url", strings.TrimRight(cfg.publicURL, "/")+"/login")
	}
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}

type devConfig struct {
	addr                string
	databaseURL         string
	casDir              string
	publicURL           string
	authSecret          string
	bootstrapOwnerEmail string
	workerTokenSecret   string
	secretEncryptionKey string
	resetDatabase       bool
}

func loadConfig() (devConfig, error) {
	cfg := devConfig{
		addr:                env("HELMR_CONTROL_ADDR", defaultAddr),
		databaseURL:         os.Getenv("HELMR_DATABASE_URL"),
		casDir:              env("HELMR_DEV_CAS_DIR", filepath.Join(os.TempDir(), "helmr-dev-cas")),
		publicURL:           env("HELMR_PUBLIC_URL", defaultPublicURL),
		authSecret:          env("HELMR_AUTH_SECRET", defaultAuthSecret),
		bootstrapOwnerEmail: strings.TrimSpace(os.Getenv("HELMR_BOOTSTRAP_OWNER_EMAIL")),
		workerTokenSecret:   env("HELMR_WORKER_TOKEN_SIGNING_KEY", defaultWorkerTokenSecret),
		secretEncryptionKey: env("HELMR_SECRET_ENCRYPTION_KEY", defaultSecretEncryptionKey),
		resetDatabase:       envBool("HELMR_DEV_RESET_DATABASE"),
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

func devLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, queries *db.Queries, cfg devConfig) {
	userID := mustUUID(defaultUserID)
	orgID := ids.ToPG(ids.DefaultOrgID)
	if _, err := queries.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{
		OrgID:       orgID,
		UserID:      ids.ToPG(userID),
		Role:        db.OrgMemberRoleOwner,
		DisplayName: pgtype.Text{String: "Local Developer", Valid: true},
	}); err != nil {
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
		OrgID:     orgID,
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

func seedDemoData(ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, casStore cas.Store) error {
	orgID := ids.ToPG(ids.DefaultOrgID)
	userID := ids.ToPG(mustUUID(defaultUserID))
	if _, err := pool.Exec(ctx, `
INSERT INTO users (id, display_name, primary_email)
VALUES ($1, 'Local Developer', 'dev@helmr.local')
ON CONFLICT (id) DO UPDATE
   SET display_name = EXCLUDED.display_name,
       primary_email = EXCLUDED.primary_email,
       disabled_at = NULL,
       updated_at = now()
`, userID); err != nil {
		return err
	}
	if _, err := queries.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{
		OrgID:       orgID,
		UserID:      userID,
		Role:        db.OrgMemberRoleOwner,
		DisplayName: pgtype.Text{String: "Local Developer", Valid: true},
	}); err != nil {
		return err
	}
	if _, err := queries.UpsertGitHubInstallation(ctx, db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(mustUUID("00000000-0000-0000-0000-000000000201")),
		OrgID:               orgID,
		InstallationID:      1,
		AccountLogin:        "helmrdotdev",
		AccountType:         "Organization",
		RepositorySelection: pgtype.Text{String: "selected", Valid: true},
	}); err != nil {
		return err
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, orgID)
	if err != nil {
		return err
	}
	const devGitHubRepositoryID = int64(1)
	if _, err := queries.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
		ID:                 ids.ToPG(mustUUID("00000000-0000-0000-0000-000000000202")),
		OrgID:              orgID,
		InstallationID:     1,
		GithubRepositoryID: devGitHubRepositoryID,
		OwnerLogin:         "helmrdotdev",
		Name:               "helmr",
		FullName:           "helmrdotdev/helmr",
		DefaultBranch:      pgtype.Text{String: "main", Valid: true},
	}); err != nil {
		return err
	}
	if _, err := queries.EnableGitHubRepositoryConnection(ctx, db.EnableGitHubRepositoryConnectionParams{
		ID:                 ids.ToPG(mustUUID("00000000-0000-0000-0000-000000000203")),
		OrgID:              orgID,
		GithubRepositoryID: devGitHubRepositoryID,
		EnabledByUserID:    userID,
	}); err != nil {
		return err
	}
	if _, err := queries.EnableProjectWorkspaceRepositoryAccess(ctx, db.EnableProjectWorkspaceRepositoryAccessParams{
		ID:                 ids.ToPG(mustUUID("00000000-0000-0000-0000-000000000204")),
		OrgID:              orgID,
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: devGitHubRepositoryID,
		EnabledByUserID:    userID,
	}); err != nil {
		return err
	}
	workerGroups, err := queries.ListWorkerGroupsByScope(ctx, db.ListWorkerGroupsByScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RowLimit:      1,
	})
	if err != nil {
		return err
	}
	if len(workerGroups) == 0 {
		return fmt.Errorf("default worker group is missing")
	}
	workerGroup := workerGroups[0]
	workerHostID := ids.ToPG(mustUUID("00000000-0000-0000-0000-000000000401"))
	taskIDs := []string{"queued-demo", "running-demo", "approval-demo", "completed-demo", "failed-demo"}
	taskDeploymentID, deployedTaskIDs, err := seedTaskCatalog(ctx, pool, casStore, scope.ProjectID, scope.EnvironmentID, taskIDs)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO worker_hosts (
    id, org_id, worker_group_id, external_id, status,
    total_milli_cpu, total_memory_mib, total_disk_mib, total_execution_slots,
    available_milli_cpu, available_memory_mib, available_disk_mib, available_execution_slots,
    labels, heartbeat, first_seen_at, last_seen_at
) VALUES (
    $1, $2, $3, 'dev-worker', 'active',
    2000, 2048, 20480, 1,
    2000, 2048, 20480, 1,
    '{}'::jsonb,
    '{"runtime_arch":"amd64","runtime_abi":"helmr.dev.v0","kernel_digest":"sha256:dev-kernel","rootfs_digest":"sha256:dev-rootfs","cni_profile":"helmr/dev"}'::jsonb,
    now() - interval '15 minutes', now() - interval '1 minute'
)
ON CONFLICT (org_id, worker_group_id, external_id) DO UPDATE
   SET status = EXCLUDED.status,
       total_milli_cpu = EXCLUDED.total_milli_cpu,
       total_memory_mib = EXCLUDED.total_memory_mib,
       total_disk_mib = EXCLUDED.total_disk_mib,
       total_execution_slots = EXCLUDED.total_execution_slots,
       available_milli_cpu = EXCLUDED.available_milli_cpu,
       available_memory_mib = EXCLUDED.available_memory_mib,
       available_disk_mib = EXCLUDED.available_disk_mib,
       available_execution_slots = EXCLUDED.available_execution_slots,
       heartbeat = EXCLUDED.heartbeat,
       last_seen_at = EXCLUDED.last_seen_at
`, workerHostID, orgID, workerGroup.ID); err != nil {
		return err
	}
	if err := seedRun(ctx, pool, scope.ProjectID, scope.EnvironmentID, taskDeploymentID, deployedTaskIDs["queued-demo"], workerGroup.ID, workerHostID, "00000000-0000-0000-0000-000000001001", "queued-demo", "queued", "", "", 0); err != nil {
		return err
	}
	if err := seedRun(ctx, pool, scope.ProjectID, scope.EnvironmentID, taskDeploymentID, deployedTaskIDs["running-demo"], workerGroup.ID, workerHostID, "00000000-0000-0000-0000-000000001002", "running-demo", "running", "00000000-0000-0000-0000-000000002002", "", 0); err != nil {
		return err
	}
	if err := seedRun(ctx, pool, scope.ProjectID, scope.EnvironmentID, taskDeploymentID, deployedTaskIDs["approval-demo"], workerGroup.ID, workerHostID, "00000000-0000-0000-0000-000000001003", "approval-demo", "waiting", "00000000-0000-0000-0000-000000002003", "00000000-0000-0000-0000-000000003003", 0); err != nil {
		return err
	}
	if err := seedRun(ctx, pool, scope.ProjectID, scope.EnvironmentID, taskDeploymentID, deployedTaskIDs["completed-demo"], workerGroup.ID, workerHostID, "00000000-0000-0000-0000-000000001004", "completed-demo", "succeeded", "", "", 0); err != nil {
		return err
	}
	if err := seedRun(ctx, pool, scope.ProjectID, scope.EnvironmentID, taskDeploymentID, deployedTaskIDs["failed-demo"], workerGroup.ID, workerHostID, "00000000-0000-0000-0000-000000001005", "failed-demo", "failed", "", "", 1); err != nil {
		return err
	}
	return seedLogs(ctx, pool)
}

func seedTaskCatalog(ctx context.Context, pool *pgxpool.Pool, casStore cas.Store, projectID pgtype.UUID, environmentID pgtype.UUID, taskIDs []string) (pgtype.UUID, map[string]pgtype.UUID, error) {
	orgID := ids.ToPG(ids.DefaultOrgID)
	deploymentID := ids.ToPG(mustUUID("00000000-0000-0000-0000-000000004001"))
	artifact, err := seedTaskSourceArtifact(ctx, casStore, taskIDs)
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO cas_objects (digest, size_bytes, media_type)
VALUES ($1, $2, $3)
ON CONFLICT (digest) DO UPDATE
   SET size_bytes = EXCLUDED.size_bytes
`, artifact.Digest, artifact.SizeBytes, artifact.MediaType); err != nil {
		return pgtype.UUID{}, nil, err
	}
	if _, err := pool.Exec(ctx, `
UPDATE task_deployments
   SET status = 'archived',
       archived_at = COALESCE(archived_at, now())
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND id <> $4
   AND status = 'active'
`, orgID, projectID, environmentID, deploymentID); err != nil {
		return pgtype.UUID{}, nil, err
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO task_deployments (id, org_id, project_id, environment_id, source_digest, status, created_at, deployed_at)
VALUES ($1, $2, $3, $4, $5, 'active', now() - interval '20 minutes', now() - interval '20 minutes')
ON CONFLICT (id) DO UPDATE
   SET source_digest = EXCLUDED.source_digest,
       status = EXCLUDED.status,
       deployed_at = EXCLUDED.deployed_at,
       archived_at = NULL
`, deploymentID, orgID, projectID, environmentID, artifact.Digest); err != nil {
		return pgtype.UUID{}, nil, err
	}
	deployedTaskIDs := make(map[string]pgtype.UUID, len(taskIDs))
	for index, taskID := range taskIDs {
		deployedTaskID := ids.ToPG(mustUUID(fmt.Sprintf("00000000-0000-0000-0000-000000005%03d", index+1)))
		if _, err := pool.Exec(ctx, `
INSERT INTO deployed_tasks (id, org_id, project_id, environment_id, deployment_id, task_id, module_path, export_name, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'tasks/demo.ts', $7, now() - interval '20 minutes')
ON CONFLICT (id) DO UPDATE
   SET task_id = EXCLUDED.task_id,
       module_path = EXCLUDED.module_path,
       export_name = EXCLUDED.export_name
`, deployedTaskID, orgID, projectID, environmentID, deploymentID, taskID, taskExportName(taskID)); err != nil {
			return pgtype.UUID{}, nil, err
		}
		deployedTaskIDs[taskID] = deployedTaskID
	}
	return deploymentID, deployedTaskIDs, nil
}

func seedTaskSourceArtifact(ctx context.Context, casStore cas.Store, taskIDs []string) (cas.Object, error) {
	root, err := os.MkdirTemp("", "helmr-dev-task-source-")
	if err != nil {
		return cas.Object{}, err
	}
	defer os.RemoveAll(root)
	if err := os.WriteFile(filepath.Join(root, "helmr.config.ts"), []byte("import { defineConfig } from \"@helmr/sdk\"\n\nexport default defineConfig({ dirs: [\"tasks\"] })\n"), 0o644); err != nil {
		return cas.Object{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		return cas.Object{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", "demo.ts"), []byte(seedTaskModule(taskIDs)), 0o644); err != nil {
		return cas.Object{}, err
	}
	archive, cleanup, err := sourcetar.CreateTar(root, "")
	if err != nil {
		return cas.Object{}, err
	}
	defer cleanup()
	file, err := os.Open(archive.Path)
	if err != nil {
		return cas.Object{}, err
	}
	defer file.Close()
	return casStore.Put(ctx, cas.TaskSourceArtifactMediaType, file)
}

func seedTaskModule(taskIDs []string) string {
	var builder strings.Builder
	builder.WriteString("import { image, sandbox, task } from \"@helmr/sdk\"\n\n")
	builder.WriteString("const sbx = sandbox(\"dev-demo\").image(image(\"dev-demo\").from(\"debian:trixie-slim\")).workspace(\"/workspace\")\n\n")
	for _, taskID := range taskIDs {
		fmt.Fprintf(&builder, "export const %s = task({ id: %q, sandbox: sbx, run: async () => ({ ok: true, taskId: %q }) })\n", taskExportName(taskID), taskID, taskID)
	}
	return builder.String()
}

func taskExportName(taskID string) string {
	var builder strings.Builder
	upperNext := false
	for _, r := range taskID {
		if r == '-' || r == '_' || r == '.' {
			upperNext = true
			continue
		}
		if upperNext && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		builder.WriteRune(r)
		upperNext = false
	}
	return builder.String()
}

func seedRun(ctx context.Context, pool *pgxpool.Pool, projectID pgtype.UUID, environmentID pgtype.UUID, taskDeploymentID pgtype.UUID, deployedTaskID pgtype.UUID, workerGroupID pgtype.UUID, workerHostID pgtype.UUID, runID string, taskID string, status string, executionID string, checkpointID string, exitCode int) error {
	orgID := ids.ToPG(ids.DefaultOrgID)
	runUUID := ids.ToPG(mustUUID(runID))
	var exit any
	var finishedAt any
	if status == "succeeded" || status == "failed" {
		exit = exitCode
		finishedAt = time.Now().Add(-2 * time.Minute)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, task_id, status, payload, secret_bindings,
    task_deployment_id, deployed_task_id,
    workspace_repository, workspace_installation_id, workspace_github_repository_id, workspace_ref, workspace_sha, workspace_subpath,
    max_duration_seconds, exit_code, created_at, updated_at, finished_at
) VALUES (
    $1, $2, $3, $4, $5, $6::run_status, '{}'::jsonb, '{}'::jsonb,
    $7, $8,
    'helmrdotdev/helmr', 1, 1, 'main', '0123456789abcdef0123456789abcdef01234567', '',
    900, $9, now() - interval '15 minutes', now() - interval '2 minutes', $10
)
ON CONFLICT (id) DO UPDATE
   SET status = EXCLUDED.status,
       exit_code = EXCLUDED.exit_code,
       finished_at = EXCLUDED.finished_at,
       updated_at = EXCLUDED.updated_at
`, runUUID, orgID, projectID, environmentID, taskID, status, taskDeploymentID, deployedTaskID, exit, finishedAt); err != nil {
		return err
	}
	if executionID != "" {
		executionUUID := ids.ToPG(mustUUID(executionID))
		executionStatus := "running"
		if status == "waiting" {
			executionStatus = "detached"
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO run_executions (id, org_id, run_id, worker_group_id, worker_host_id, queue_message_id, queue_lease_id, delivery_attempt, status, lease_expires_at, started_at, released_at)
VALUES (
    $1, $2, $3, $4, $5, 'dev-message-' || $3::text, 'dev-lease-' || $1::text, 1,
    $6::run_execution_status, now() + interval '10 minutes', now() - interval '12 minutes',
    CASE WHEN $6::run_execution_status = 'detached' THEN now() - interval '1 minute' ELSE NULL END
)
ON CONFLICT (id) DO UPDATE
   SET status = EXCLUDED.status,
       worker_group_id = EXCLUDED.worker_group_id,
       worker_host_id = EXCLUDED.worker_host_id,
       queue_message_id = EXCLUDED.queue_message_id,
       queue_lease_id = EXCLUDED.queue_lease_id,
       lease_expires_at = EXCLUDED.lease_expires_at,
       released_at = EXCLUDED.released_at
`, executionUUID, orgID, runUUID, workerGroupID, workerHostID, executionStatus); err != nil {
			return err
		}
		if status != "waiting" {
			if _, err := pool.Exec(ctx, `UPDATE runs SET current_execution_id = $1 WHERE id = $2`, executionUUID, runUUID); err != nil {
				return err
			}
		}
	}
	if checkpointID != "" {
		checkpointUUID := ids.ToPG(mustUUID(checkpointID))
		executionUUID := ids.ToPG(mustUUID(executionID))
		if _, err := pool.Exec(ctx, `
INSERT INTO checkpoints (id, org_id, run_id, execution_id, status, reason, runtime_backend, runtime_arch, runtime_abi, ready_at)
VALUES ($1, $2, $3, $4, 'ready', 'wait_approval', 'dev', 'amd64', 'helmr.dev.v0', now() - interval '1 minute')
ON CONFLICT (id) DO UPDATE
   SET status = EXCLUDED.status,
       ready_at = EXCLUDED.ready_at
`, checkpointUUID, orgID, runUUID, executionUUID); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO waitpoints (
    id, org_id, run_id, execution_id, checkpoint_id, correlation_id,
    kind, request, display_text, status, requested_at
) VALUES (
    $1, $2, $3, $4, $5, 'dev-approval',
    'approval', '{"message":"Approve the demo deployment?"}'::jsonb, 'Approve the demo deployment?', 'pending', now() - interval '1 minute'
)
ON CONFLICT (id) DO UPDATE
   SET status = 'pending',
       display_text = EXCLUDED.display_text,
       requested_at = EXCLUDED.requested_at,
       resolution_kind = NULL,
       resolution = NULL,
       resolved_at = NULL
`, ids.ToPG(mustUUID("00000000-0000-0000-0000-000000004003")), orgID, runUUID, executionUUID, checkpointUUID); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `UPDATE runs SET latest_checkpoint_id = $1 WHERE id = $2`, checkpointUUID, runUUID); err != nil {
			return err
		}
	}
	return nil
}

func seedLogs(ctx context.Context, pool *pgxpool.Pool) error {
	rows := []struct {
		runID       string
		executionID string
		stream      string
		seq         int
		content     string
	}{
		{"00000000-0000-0000-0000-000000001002", "00000000-0000-0000-0000-000000002002", "stdout", 1, "starting dev run\ninstalling dependencies\n"},
		{"00000000-0000-0000-0000-000000001002", "00000000-0000-0000-0000-000000002002", "stderr", 1, "warning: demo worker uses fake GitHub resolver\n"},
	}
	for _, row := range rows {
		if _, err := pool.Exec(ctx, `
INSERT INTO run_log_chunks (run_id, execution_id, stream, seq, observed_seq, content)
VALUES ($1, $2, $3::run_log_stream, $4, $4, $5)
ON CONFLICT (run_id, stream, seq) DO UPDATE
   SET content = EXCLUDED.content
`, ids.ToPG(mustUUID(row.runID)), ids.ToPG(mustUUID(row.executionID)), row.stream, row.seq, []byte(row.content)); err != nil {
			return err
		}
	}
	return nil
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
