package schedule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDBAdmitterCommitsOneScheduleAdmissionTuple(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, runtimeDigest := seedScheduleAdmission(t, pool)
	admission, err := BuildAdmissionAt(value, value.NextFireAt.Time)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := NewDBAdmitter(pool, fixedAuthority{digest: runtimeDigest})
	if err != nil {
		t.Fatal(err)
	}
	admitter.now = func() time.Time { return value.NextFireAt.Time }

	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION reject_run_admission_outbox() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.topic = 'run.admit' THEN
				RAISE EXCEPTION 'reject Run admission outbox';
			END IF;
			RETURN NEW;
		END;
		$$;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		CREATE TRIGGER reject_run_admission_outbox
		BEFORE INSERT ON outbox_messages
		FOR EACH ROW EXECUTE FUNCTION reject_run_admission_outbox();
	`); err != nil {
		t.Fatal(err)
	}
	if err := admitter.AdmitSchedule(t.Context(), value); err == nil {
		t.Fatal("expected rejected outbox to roll admission back")
	}
	assertScheduleAdmissionCounts(t, pool, value, 0, 0, 0)
	assertScheduleCursor(t, pool, value, value.NextFireAt.Time, time.Time{})

	if _, err := pool.Exec(t.Context(), `DROP TRIGGER reject_run_admission_outbox ON outbox_messages`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `DROP FUNCTION reject_run_admission_outbox()`); err != nil {
		t.Fatal(err)
	}
	if err := admitter.AdmitSchedule(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 1, 1, 1)
	assertScheduleCursor(t, pool, value, admission.NextFireAt, admission.ScheduledAt)

	if err := admitter.AdmitSchedule(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 1, 1, 1)
}

func TestDBAdmitterRejectsTaskWithoutScheduledPayloadAuthority(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, runtimeDigest := seedScheduleAdmission(t, pool)
	mustScheduleExec(t, pool, `
		UPDATE deployment_definitions
		   SET manifest = '{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}',
		       manifest_digest = decode(repeat('0a', 32), 'hex')
		 WHERE environment_id = $1
		   AND kind = 'task'
		   AND declared_id = 'daily-report'
	`, value.EnvironmentID)
	admitter, err := NewDBAdmitter(pool, fixedAuthority{digest: runtimeDigest})
	if err != nil {
		t.Fatal(err)
	}
	admitter.now = func() time.Time { return value.NextFireAt.Time }
	err = admitter.AdmitSchedule(t.Context(), value)
	var admissionErr *AdmissionError
	if !errors.As(err, &admissionErr) ||
		admissionErr.Code != ErrorTaskAuthorityInvalid {
		t.Fatalf("admission error = %v, want task-authority-invalid", err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 0, 0, 0)
	assertScheduleCursor(t, pool, value, value.NextFireAt.Time, time.Time{})
}

func TestPendingScheduleBindsMatchingWorkspaceWithGenerationFence(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, _ := seedScheduleAdmission(t, pool)
	mustScheduleExec(t, pool, `
		UPDATE schedules
		   SET state = 'pending_workspace',
		       workspace_id = NULL,
		       next_fire_at = NULL,
		       claimed_by = NULL,
		       claim_expires_at = NULL
		 WHERE id = $1
	`, value.ID)
	queries := db.New(pool)
	pending, err := queries.ListPendingScheduleBindings(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != value.ID ||
		pending[0].ResolvedWorkspaceID != value.WorkspaceID {
		t.Fatalf("pending bindings = %+v", pending)
	}
	effectiveFrom := time.Date(2026, 7, 24, 3, 2, 0, 0, time.UTC)
	nextFireAt := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	activated, err := queries.ActivatePendingSchedule(
		t.Context(),
		db.ActivatePendingScheduleParams{
			WorkspaceID:        pending[0].ResolvedWorkspaceID,
			EffectiveFrom:      pgvalue.Timestamptz(effectiveFrom),
			NextFireAt:         pgvalue.Timestamptz(nextFireAt),
			EnvironmentID:      value.EnvironmentID,
			ID:                 value.ID,
			ExpectedGeneration: value.Generation,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated.State != "active" ||
		activated.Generation != value.Generation+1 ||
		activated.WorkspaceID != value.WorkspaceID ||
		!activated.NextFireAt.Time.Equal(nextFireAt) {
		t.Fatalf("activated Schedule = %+v", activated)
	}
}

func TestReconcileScheduleDoesNotReviveErroredAuthority(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, _ := seedScheduleAdmission(t, pool)
	lastError := json.RawMessage(`{"code":"task-authority-invalid","message":"Task authority is invalid"}`)
	mustScheduleExec(t, pool, `
		UPDATE schedules
		   SET state = 'errored',
		       state_version = state_version + 1,
		       claimed_by = NULL,
		       claim_expires_at = NULL,
		       last_error = $2
		 WHERE id = $1
	`, value.ID, lastError)

	queries := db.New(pool)
	before, err := queries.GetSchedule(t.Context(), db.GetScheduleParams{
		EnvironmentID: value.EnvironmentID,
		ID:            value.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.ReconcileSchedule(t.Context(), db.ReconcileScheduleParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:               schedulePublicID(t, publicid.Schedule),
		OrgID:                  before.OrgID,
		ProjectID:              before.ProjectID,
		EnvironmentID:          before.EnvironmentID,
		TaskDeclaredID:         before.TaskDeclaredID,
		DeploymentDefinitionID: before.DeploymentDefinitionID,
		DeploymentID:           before.DeploymentID,
		WorkspaceRefID:         before.WorkspaceRefID,
		WorkspaceRefKey:        before.WorkspaceRefKey,
		WorkspaceID:            before.WorkspaceID,
		CronPattern:            before.CronPattern,
		Timezone:               before.Timezone,
		CronSemanticsVersion:   before.CronSemanticsVersion,
		State:                  "active",
		EffectiveFrom:          before.EffectiveFrom,
		NextFireAt:             before.NextFireAt,
	}); err != nil {
		t.Fatal(err)
	}

	after, err := queries.GetSchedule(t.Context(), db.GetScheduleParams{
		EnvironmentID: value.EnvironmentID,
		ID:            value.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "errored" ||
		after.Generation != before.Generation ||
		after.StateVersion != before.StateVersion ||
		string(after.LastError) != string(before.LastError) {
		t.Fatalf(
			"reconciled errored Schedule = state %q, generation %d, state version %d, error %s; want %q, %d, %d, %s",
			after.State,
			after.Generation,
			after.StateVersion,
			after.LastError,
			before.State,
			before.Generation,
			before.StateVersion,
			before.LastError,
		)
	}
}

func seedScheduleAdmission(t *testing.T, pool *pgxpool.Pool) (db.Schedule, string) {
	t.Helper()
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	deploymentID := uuid.Must(uuid.NewV7())
	taskDefinitionID := uuid.Must(uuid.NewV7())
	workspaceDefinitionID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	workspaceVersionID := uuid.Must(uuid.NewV7())
	scheduleID := uuid.Must(uuid.NewV7())
	secretID := uuid.Must(uuid.NewV7())
	secretVersionID := uuid.Must(uuid.NewV7())
	regionID := "schedule-" + environmentID.String()

	mustScheduleExec(t, pool, `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Schedules', $3)
	`, orgID, schedulePublicID(t, publicid.Organization), "schedules-"+orgID.String())
	mustScheduleExec(t, pool, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'test', $1, 'Schedules')
	`, regionID)
	mustScheduleExec(t, pool, `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, $5, 'Schedules')
	`, projectID, schedulePublicID(t, publicid.Project), orgID, regionID, "schedules-"+projectID.String())
	mustScheduleExec(t, pool, `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'production', 'Production', '#000000')
	`, environmentID, schedulePublicID(t, publicid.Environment), orgID, projectID)

	sourceArtifactID := seedScheduleArtifact(t, pool, orgID, projectID, environmentID, "deployment_source", "source")
	programArtifactID := seedScheduleArtifact(t, pool, orgID, projectID, environmentID, "deployment_program", "program")
	imageArtifactID := seedScheduleArtifact(t, pool, orgID, projectID, environmentID, "workspace_image", "image")
	runtimeBytes := strings.Repeat("01", 32)
	runtimeDigest := "sha256:" + runtimeBytes
	queueConfig := `{"formatVersion":0,"queues":[{"name":"default"}]}`
	sourceSum := sha256.Sum256([]byte("source"))
	programSum := sha256.Sum256([]byte("program"))
	programReceipt := dbtest.ProgramReceipt(dbtest.ProgramReceiptAuthority{
		Architecture:            "x86_64",
		ProgramArtifactID:       programArtifactID,
		ProgramDigest:           "sha256:" + hex.EncodeToString(programSum[:]),
		ProgramSizeBytes:        1,
		RuntimeDigest:           runtimeDigest,
		SourceArtifactID:        sourceArtifactID,
		SourceDigest:            "sha256:" + hex.EncodeToString(sourceSum[:]),
		SourceSizeBytes:         1,
		StandardToolchainDigest: "sha256:" + strings.Repeat("02", 32),
	})
	mustScheduleExec(t, pool, `
		INSERT INTO deployments (
			id, public_id, org_id, project_id, environment_id, build_region_id,
			build_architecture, build_runtime_digest, build_standard_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, version, content_hash, deployment_source_artifact_id,
			program_artifact_id, program_runtime_digest, program_architecture,
			program_receipt, queue_config, status
		)
		VALUES (
			$1, $2, $3, $4, $5, $6,
			'x86_64', decode($7, 'hex'), decode(repeat('02', 32), 'hex'),
			'bun', '1.2.3', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'v0', $8, $9,
			$10, decode($7, 'hex'), 'x86_64', $11::jsonb, $12, 'deployed'
		)
	`, deploymentID, schedulePublicID(t, publicid.Deployment), orgID, projectID,
		environmentID, regionID, runtimeBytes,
		"sha256:"+strings.Repeat("03", 32), sourceArtifactID, programArtifactID,
		programReceipt, queueConfig)
	taskManifest := []byte(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"key":"scheduler"}}}`,
	)
	taskManifestHash := sha256.New()
	_, _ = taskManifestHash.Write([]byte("helmr.deployment-definition-manifest.v0\x00"))
	_, _ = taskManifestHash.Write(taskManifest)
	mustScheduleExec(t, pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest
		)
		VALUES ($1, $2, $3, 'task', 'daily-report', 0, $4, $5)
	`, taskDefinitionID, environmentID, deploymentID, taskManifest, taskManifestHash.Sum(nil))
	mustScheduleExec(t, pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, workspace_architecture, artifact_id
		)
		VALUES ($1, $2, $3, 'workspace', 'scheduler', 0, '{}',
		        decode(repeat('05', 32), 'hex'), 'x86_64', $4)
	`, workspaceDefinitionID, environmentID, deploymentID, imageArtifactID)
	mustScheduleExec(t, pool, `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, deploymentID, environmentID)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO workspaces (
			id, public_id, org_id, project_id, environment_id, region_id,
			declaration_kind, workspace_declared_id, deployment_definition_id,
			head_version_id, key
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'workspace', 'scheduler', $7, $8, 'scheduler')
	`, workspaceID, schedulePublicID(t, publicid.Workspace), orgID, projectID,
		environmentID, regionID, workspaceDefinitionID, workspaceVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id,
			kind, state, content_digest, size_bytes, entry_count,
			ownership_generation, writer_generation, published_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'system', 'committed',
		        'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
		        0, 0, 0, 0, now())
	`, workspaceVersionID, schedulePublicID(t, publicid.WorkspaceVersion), orgID,
		projectID, environmentID, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	mustScheduleExec(t, pool, `
		INSERT INTO lookup_hmac_versions (version, key_fingerprint, is_current)
		VALUES (1, decode(repeat('06', 32), 'hex'), true)
	`)
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secrets (id, environment_id, name, current_version_id)
		VALUES ($1, $2, 'API_TOKEN', $3)
	`, secretID, environmentID, secretVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secret_versions (
			id, secret_id, version, key_id, nonce, ciphertext,
			value_authenticator, authenticator_key_version
		)
		VALUES ($1, $2, 1, 'key', decode(repeat('07', 12), 'hex'),
		        decode(repeat('08', 16), 'hex'), decode(repeat('09', 32), 'hex'), 1)
	`, secretVersionID, secretID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustScheduleExec(t, pool, `
		INSERT INTO workspace_secrets (
			workspace_id, environment_id, placement_kind, placement_target, secret_id
		)
		VALUES ($1, $2, 'env', 'API_TOKEN', $3)
	`, workspaceID, environmentID, secretID)

	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	claimExpiresAt := time.Now().UTC().Add(5 * time.Minute)
	mustScheduleExec(t, pool, `
		INSERT INTO schedules (
			id, public_id, org_id, project_id, environment_id,
			task_declared_id, deployment_definition_id, deployment_id,
			workspace_ref_key, workspace_id, cron_pattern, timezone,
			state, effective_from,
			next_fire_at, claimed_by, claim_expires_at
		)
		VALUES (
			$1, $2, $3, $4, $5,
			'daily-report', $6, $7,
			'scheduler', $8, '0 9 * * *', 'UTC',
			'active', now() - interval '1 hour',
			$9, 'scheduler-test', $10
		)
	`, scheduleID, schedulePublicID(t, publicid.Schedule), orgID, projectID,
		environmentID, taskDefinitionID, deploymentID, workspaceID,
		scheduledAt, claimExpiresAt)
	value, err := db.New(pool).GetSchedule(t.Context(), db.GetScheduleParams{
		EnvironmentID: pgvalue.UUID(environmentID),
		ID:            pgvalue.UUID(scheduleID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return value, runtimeDigest
}

func seedScheduleArtifact(
	t *testing.T,
	pool *pgxpool.Pool,
	orgID uuid.UUID,
	projectID uuid.UUID,
	environmentID uuid.UUID,
	kind string,
	seed string,
) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	sum := sha256.Sum256([]byte(seed))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	mediaType := "application/octet-stream"
	switch kind {
	case "deployment_source":
		mediaType = "application/vnd.helmr.deployment-source.v0+tar"
	case "deployment_program":
		mediaType = "application/vnd.helmr.deployment-program.v0+squashfs"
	}
	mustScheduleExec(t, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, $3)
	`, orgID, digest, mediaType)
	mustScheduleExec(t, pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		)
		VALUES ($1, $2, $3, $4, $5, $6::artifact_kind, 1, $7)
	`, id, orgID, projectID, environmentID, digest, kind, mediaType)
	return id
}

func assertScheduleAdmissionCounts(
	t *testing.T,
	pool *pgxpool.Pool,
	value db.Schedule,
	runs int,
	resolutions int,
	outboxes int,
) {
	t.Helper()
	var runCount, attemptCount, resolutionCount, outboxCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM runs WHERE schedule_id = $1
	`, value.ID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM run_attempts
		 WHERE run_id IN (SELECT id FROM runs WHERE schedule_id = $1)
	`, value.ID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM secret_resolutions
		 WHERE run_id IN (SELECT id FROM runs WHERE schedule_id = $1)
	`, value.ID).Scan(&resolutionCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM outbox_messages WHERE topic = 'run.admit'
	`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if runCount != runs || attemptCount != runs || resolutionCount != resolutions || outboxCount != outboxes {
		t.Fatalf(
			"runs/attempts/resolutions/outboxes = %d/%d/%d/%d, want %d/%d/%d/%d",
			runCount,
			attemptCount,
			resolutionCount,
			outboxCount,
			runs,
			runs,
			resolutions,
			outboxes,
		)
	}
	var ownerRunID *uuid.UUID
	var stateVersion, ownershipGeneration int64
	if err := pool.QueryRow(t.Context(), `
		SELECT owner_run_id, state_version, ownership_generation
		  FROM workspaces
		 WHERE id = $1
	`, value.WorkspaceID).Scan(&ownerRunID, &stateVersion, &ownershipGeneration); err != nil {
		t.Fatal(err)
	}
	if (runs == 0) != (ownerRunID == nil) {
		t.Fatalf("Workspace owner_run_id = %v for %d Runs", ownerRunID, runs)
	}
	if runs == 0 && (stateVersion != 1 || ownershipGeneration != 0) {
		t.Fatalf(
			"rolled-back Workspace state/ownership generations = %d/%d, want 1/0",
			stateVersion,
			ownershipGeneration,
		)
	}
	if runs == 1 && (stateVersion != 2 || ownershipGeneration != 1) {
		t.Fatalf(
			"reserved Workspace state/ownership generations = %d/%d, want 2/1",
			stateVersion,
			ownershipGeneration,
		)
	}
}

func assertScheduleCursor(
	t *testing.T,
	pool *pgxpool.Pool,
	value db.Schedule,
	next time.Time,
	last time.Time,
) {
	t.Helper()
	var nextFireAt time.Time
	var lastFireAt *time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT next_fire_at, last_fire_at FROM schedules WHERE id = $1
	`, value.ID).Scan(&nextFireAt, &lastFireAt); err != nil {
		t.Fatal(err)
	}
	if !nextFireAt.Equal(next) {
		t.Fatalf("next_fire_at = %s, want %s", nextFireAt, next)
	}
	if last.IsZero() {
		if lastFireAt != nil {
			t.Fatalf("last_fire_at = %s, want NULL", *lastFireAt)
		}
	} else if lastFireAt == nil || !lastFireAt.Equal(last) {
		t.Fatalf("last_fire_at = %v, want %s", lastFireAt, last)
	}
}

func mustScheduleExec(t *testing.T, pool *pgxpool.Pool, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), query, arguments...); err != nil {
		t.Fatal(err)
	}
}

func schedulePublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	value, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

type fixedAuthority struct {
	digest string
}

func (r fixedAuthority) ResolveRuntime(value string) error {
	if value != r.digest {
		return errors.New("runtime is unavailable")
	}
	return nil
}

func (fixedAuthority) ResolveScheduledTask(
	manifestVersion int32,
	declaredID string,
	manifest []byte,
	manifestDigest []byte,
	queueConfig []byte,
) (TaskRun, error) {
	var value struct {
		Payload struct {
			Kind string `json:"kind"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(manifest, &value); err != nil {
		return TaskRun{}, err
	}
	if manifestVersion != 0 ||
		declaredID != "daily-report" ||
		value.Payload.Kind != "standard_schema" ||
		len(manifestDigest) != sha256.Size ||
		len(queueConfig) == 0 {
		return TaskRun{}, errors.New("scheduled Task authority is invalid")
	}
	return TaskRun{
		QueueName:           "default",
		MaxActiveDurationMS: 300000,
		RetryPolicy:         []byte(`{"enabled":false}`),
	}, nil
}

func openSchedulePostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping PostgreSQL Schedule test", name)
		}
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeSchedulePostgresPort(t)
	logPath := filepath.Join(filepath.Dir(dataDir), "postgres.log")
	command := exec.Command(
		"pg_ctl",
		"-D",
		dataDir,
		"-l",
		logPath,
		"-o",
		fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port),
		"-w",
		"start",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})
	dsn := fmt.Sprintf(
		"postgres://%s@127.0.0.1:%d/postgres?sslmode=disable",
		os.Getenv("USER"),
		port,
	)
	if err := schema.Up(t.Context(), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freeSchedulePostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
