package schedule

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
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

	if err := admitter.AdmitSchedule(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 1, 1)
	assertScheduleCursor(t, pool, value, admission.NextFireAt, admission.ScheduledAt)
	receipt, err := db.New(pool).GetScheduledRunReceipt(t.Context(), db.GetScheduledRunReceiptParams{
		EnvironmentID: value.EnvironmentID,
		ScheduleID:    value.ID,
		ScheduledAt:   value.NextFireAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := admitter.AdmitSchedule(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 1, 1)

	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE schedules
		   SET claimed_by = 'scheduler-test-2',
		       claim_expires_at = now() + interval '5 minutes'
		 WHERE id = $1
	`, value.ID)
	second, err := db.New(pool).GetSchedule(t.Context(), db.GetScheduleParams{
		EnvironmentID: value.EnvironmentID, ID: value.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	admitter.now = func() time.Time { return second.NextFireAt.Time }
	if err := admitter.AdmitSchedule(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 2, 2)
	secondReceipt, err := db.New(pool).GetScheduledRunReceipt(t.Context(), db.GetScheduledRunReceiptParams{
		EnvironmentID: value.EnvironmentID,
		ScheduleID:    value.ID,
		ScheduledAt:   second.NextFireAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondReceipt.ID == receipt.ID {
		t.Fatalf("second scheduled Run = %s, want a new Run", secondReceipt.ID)
	}
	if secondReceipt.WorkspaceID == receipt.WorkspaceID {
		t.Fatalf("second scheduled Workspace = %s, want a fresh Workspace", secondReceipt.WorkspaceID)
	}
	var distinctWorkspaces int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(DISTINCT workspace_id) FROM runs WHERE schedule_id = $1
	`, value.ID).Scan(&distinctWorkspaces); err != nil {
		t.Fatal(err)
	}
	if distinctWorkspaces != 2 {
		t.Fatalf("distinct scheduled Workspaces = %d, want 2", distinctWorkspaces)
	}
}

func TestReconcileScheduleLocksAnUnchangedSchedule(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, _ := seedScheduleAdmission(t, pool)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := db.New(tx).ReconcileSchedule(t.Context(), db.ReconcileScheduleParams{
		ID:                     pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:          value.EnvironmentID,
		TaskDeclaredID:         value.TaskDeclaredID,
		DeploymentDefinitionID: value.DeploymentDefinitionID,
		DeploymentID:           value.DeploymentID,
		CronPattern:            value.CronPattern,
		Timezone:               value.Timezone,
		CronSemanticsVersion:   value.CronSemanticsVersion,
		State:                  value.State,
		EffectiveFrom:          value.EffectiveFrom,
		NextFireAt:             value.NextFireAt,
	}); err != nil {
		t.Fatal(err)
	}

	contender, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Rollback(t.Context())
	if _, err := contender.Exec(t.Context(), `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := contender.Exec(t.Context(), `
		UPDATE schedules
		   SET updated_at = updated_at
		 WHERE environment_id = $1
		   AND id = $2
	`, value.EnvironmentID, value.ID); err == nil {
		t.Fatal("unchanged Schedule reconciliation did not retain the row lock")
	}
}

func TestDBAdmitterRejectsTaskWithoutScheduledPayloadAuthority(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, runtimeDigest := seedScheduleAdmission(t, pool)
	dbtest.MustExec(t, t.Context(), pool, `
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
		t.Fatalf("admission error = %v, want task_authority_invalid", err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 0, 0)
	assertScheduleCursor(t, pool, value, value.NextFireAt.Time, time.Time{})
}

func TestWorkerRetriesSameScheduleInstantWhenWorkspaceSecretIsRevoked(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, runtimeDigest := seedScheduleAdmission(t, pool)
	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE secrets
		   SET state = 'revoked',
		       current_version_id = NULL,
		       revocation_generation = revocation_generation + 1,
		       revoked_at = now(),
		       updated_at = now()
		 WHERE id = (
		       SELECT secret_id
		         FROM schedule_secrets
		        WHERE schedule_id = $1
		        LIMIT 1
		 )
	`, value.ID)
	admitter, err := NewDBAdmitter(pool, fixedAuthority{digest: runtimeDigest})
	if err != nil {
		t.Fatal(err)
	}
	admitter.now = func() time.Time { return value.NextFireAt.Time }
	worker, err := NewWorker(nil, db.New(pool), admitter)
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return value.NextFireAt.Time }
	worker.jitter = func(time.Duration) (time.Duration, error) { return 0, nil }

	err = worker.process(t.Context(), value)
	var permanent *AdmissionError
	if err == nil || errors.As(err, &permanent) {
		t.Fatalf("Secret-unavailable admission error = %v, want retryable non-AdmissionError", err)
	}
	assertScheduleAdmissionCounts(t, pool, value, 0, 0)
	assertScheduleCursor(t, pool, value, value.NextFireAt.Time, time.Time{})
	after, err := db.New(pool).GetSchedule(t.Context(), db.GetScheduleParams{
		EnvironmentID: value.EnvironmentID,
		ID:            value.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "active" || !after.RetryStep.Valid || !after.RetryAfter.Valid ||
		len(after.LastFailure) != 0 ||
		after.ClaimedBy.Valid || after.ClaimExpiresAt.Valid {
		t.Fatalf("retryable Schedule state = %+v", after)
	}
}

func TestReconcileScheduleDoesNotReviveErroredAuthority(t *testing.T) {
	pool := openSchedulePostgres(t)
	value, _ := seedScheduleAdmission(t, pool)
	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE schedules
		   SET state = 'errored',
		       state_version = state_version + 1,
		       claimed_by = NULL,
		       claim_expires_at = NULL,
		       last_failure = '{"code":"task_authority_invalid","message":"Task authority is invalid","details":{}}'::jsonb
		 WHERE id = $1
	`, value.ID)

	queries := db.New(pool)
	before, err := queries.GetSchedule(t.Context(), db.GetScheduleParams{
		EnvironmentID: value.EnvironmentID,
		ID:            value.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReconcileSchedule(t.Context(), db.ReconcileScheduleParams{
		ID:                     pgvalue.UUID(uuid.NewV7()),
		EnvironmentID:          before.EnvironmentID,
		TaskDeclaredID:         before.TaskDeclaredID,
		DeploymentDefinitionID: before.DeploymentDefinitionID,
		DeploymentID:           before.DeploymentID,
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
		string(after.LastFailure) != string(before.LastFailure) {
		t.Fatalf(
			"reconciled errored Schedule = state %q, generation %d, state version %d, failure %s; want %q, %d, %d, %s",
			after.State,
			after.Generation,
			after.StateVersion,
			after.LastFailure,
			before.State,
			before.Generation,
			before.StateVersion,
			before.LastFailure,
		)
	}
}

func seedScheduleAdmission(t *testing.T, pool *pgxpool.Pool) (db.Schedule, string) {
	t.Helper()
	orgID := uuid.NewV7()
	projectID := uuid.NewV7()
	environmentID := uuid.NewV7()
	deploymentID := uuid.NewV7()
	taskDefinitionID := uuid.NewV7()
	workspaceDefinitionID := uuid.NewV7()
	scheduleID := uuid.NewV7()
	secretID := uuid.NewV7()
	secretVersionID := uuid.NewV7()
	regionID := "schedule-" + environmentID.String()

	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Schedules', $2)
	`, orgID, "schedules-"+orgID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO regions (id, display_name)
		VALUES ($1, 'Schedules')
	`, regionID)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, 'Schedules')
	`, projectID, orgID, regionID, "schedules-"+projectID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, 'production', 'Production', '#000000')
	`, environmentID, orgID, projectID)

	programArtifactID := seedScheduleArtifact(t, pool, orgID, projectID, environmentID, "deployment_program", "program")
	imageArtifactID := seedScheduleArtifact(t, pool, orgID, projectID, environmentID, "workspace_image", "image")
	runtimeBytes := strings.Repeat("01", 32)
	runtimeDigest := "sha256:" + runtimeBytes
	queueConfig := `{"formatVersion":0,"queues":[{"name":"default"}]}`
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployments (
			id, org_id, project_id, environment_id, version, bundle_digest,
			runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		)
		VALUES (
			$1, $2, $3, $4, 'v0', $5, $6, $7,
			decode(repeat('03', 32), 'hex'), $8
		)
	`, deploymentID, orgID, projectID, environmentID,
		"sha256:"+strings.Repeat("03", 32), runtimeDigest, programArtifactID, queueConfig)
	taskManifest := []byte(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"scheduler","secrets":[{"env":"API_TOKEN","name":"API_TOKEN"}]}}}`,
	)
	taskManifestHash := sha256.New()
	_, _ = taskManifestHash.Write([]byte("helmr.deployment-definition-manifest.v0\x00"))
	_, _ = taskManifestHash.Write(taskManifest)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest
		)
		VALUES ($1, $2, $3, 'task', 'daily-report', 0, $4, $5)
	`, taskDefinitionID, environmentID, deploymentID, taskManifest, taskManifestHash.Sum(nil))
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest, artifact_id
		)
		VALUES ($1, $2, $3, 'sandbox', 'scheduler', 0, '{}',
		        decode(repeat('05', 32), 'hex'), $4)
	`, workspaceDefinitionID, environmentID, deploymentID, imageArtifactID)
	dbtest.MustExec(t, t.Context(), pool, `
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
		INSERT INTO secrets (id, environment_id, name, current_version_id)
		VALUES ($1, $2, 'API_TOKEN', $3)
	`, secretID, environmentID, secretVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO secret_versions (
			id, secret_id, version, nonce, ciphertext
		)
		VALUES ($1, $2, 1, decode(repeat('07', 12), 'hex'),
		        decode(repeat('08', 16), 'hex'))
	`, secretVersionID, secretID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	scheduledAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	claimExpiresAt := time.Now().UTC().Add(5 * time.Minute)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO schedules (
			id, environment_id,
			task_declared_id, deployment_definition_id, deployment_id,
			cron_pattern, timezone,
			state, effective_from,
			next_fire_at, claimed_by, claim_expires_at
		)
		VALUES (
			$1, $2,
			'daily-report', $3, $4,
			'0 9 * * *', 'UTC',
			'active', now() - interval '1 hour',
			$5, 'scheduler-test', $6
		)
	`, scheduleID, environmentID, taskDefinitionID, deploymentID,
		scheduledAt, claimExpiresAt)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO schedule_secrets (
			schedule_id, environment_id, placement_kind, placement_target, secret_id
		)
		VALUES ($1, $2, 'env', 'API_TOKEN', $3)
	`, scheduleID, environmentID, secretID)
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
	id := uuid.NewV7()
	digest := dbtest.Digest(seed)
	mediaType := "application/octet-stream"
	switch kind {
	case "deployment_program":
		mediaType = "application/vnd.helmr.deployment-program.v0+squashfs"
	}
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, $3)
	`, orgID, digest, mediaType)
	dbtest.MustExec(t, t.Context(), pool, `
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
) {
	t.Helper()
	var runCount, attemptCount, resolutionCount int
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
	if runCount != runs || attemptCount != runs || resolutionCount != resolutions {
		t.Fatalf(
			"runs/attempts/resolutions = %d/%d/%d, want %d/%d/%d",
			runCount,
			attemptCount,
			resolutionCount,
			runs,
			runs,
			resolutions,
		)
	}
	var workspaceCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM workspaces WHERE environment_id = $1
	`, value.EnvironmentID).Scan(&workspaceCount); err != nil {
		t.Fatal(err)
	}
	if workspaceCount != runs {
		t.Fatalf("Workspaces = %d, want %d", workspaceCount, runs)
	}
	if runs == 0 {
		return
	}
	var owned, minStateVersion, maxStateVersion, minOwnershipGeneration, maxOwnershipGeneration int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE owner_run_id IS NOT NULL),
		       min(state_version), max(state_version),
		       min(ownership_generation), max(ownership_generation)
		  FROM workspaces
		 WHERE environment_id = $1
	`, value.EnvironmentID).Scan(
		&owned, &minStateVersion, &maxStateVersion,
		&minOwnershipGeneration, &maxOwnershipGeneration,
	); err != nil {
		t.Fatal(err)
	}
	if owned != int64(runs) || minStateVersion != 2 || maxStateVersion != 2 ||
		minOwnershipGeneration != 1 || maxOwnershipGeneration != 1 {
		t.Fatalf(
			"reserved Workspaces owned/state/ownership = %d/%d-%d/%d-%d, want %d/2-2/1-1",
			owned, minStateVersion, maxStateVersion,
			minOwnershipGeneration, maxOwnershipGeneration, runs,
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

type fixedAuthority struct {
	digest string
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
		return TaskRun{}, errors.New("scheduled task authority is invalid")
	}
	return TaskRun{
		QueueName:           "default",
		MaxActiveDurationMS: 300000,
		RetryPolicy:         []byte(`{"enabled":false}`),
		SandboxDeclaredID:   "scheduler",
		SecretPlacements: []workspace.SecretPlacement{{
			Name: "API_TOKEN", Kind: "env", Target: "API_TOKEN",
		}},
	}, nil
}

func openSchedulePostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	return database.Pool
}
