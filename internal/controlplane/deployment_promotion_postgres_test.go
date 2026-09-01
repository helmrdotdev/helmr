package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type deploymentPromotionPostgresFixture struct {
	pool          *pgxpool.Pool
	server        *Server
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	otherEnvID    uuid.UUID
	olderID       uuid.UUID
	currentID     uuid.UUID
	scheduledID   uuid.UUID
	otherID       uuid.UUID
}

func TestPromoteDeploymentPostgres(t *testing.T) {
	fixture := newDeploymentPromotionPostgresFixture(t)
	principal := fixture.apiKeyPrincipal()

	t.Run("invalid and cross-scope targets fail", func(t *testing.T) {
		fixture.setCurrent(t, fixture.currentID)
		recorder := fixture.promote(t, uuid.NewV7(), principal, "", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("invalid target status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if got := fixture.currentDeployment(t); got != fixture.currentID {
			t.Fatalf("current after invalid target = %s, want %s", got, fixture.currentID)
		}

		recorder = fixture.promote(t, fixture.otherID, principal, "", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("cross-scope status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if got := fixture.currentDeployment(t); got != fixture.currentID {
			t.Fatalf("current after cross-scope = %s, want %s", got, fixture.currentID)
		}
	})

	t.Run("schedule reconciliation rolls back", func(t *testing.T) {
		fixture.setCurrent(t, fixture.currentID)
		recorder := fixture.promote(t, fixture.scheduledID, principal, "", "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "REPORT_TOKEN") {
			t.Fatalf("error = %s, want unavailable scheduled secret", recorder.Body.String())
		}
		if got := fixture.currentDeployment(t); got != fixture.currentID {
			t.Fatalf("current after failed reconciliation = %s, want %s", got, fixture.currentID)
		}
		var schedules int
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM schedules WHERE environment_id = $1
		`, fixture.environmentID).Scan(&schedules); err != nil {
			t.Fatal(err)
		}
		if schedules != 0 {
			t.Fatalf("schedules = %d, want 0 after rollback", schedules)
		}
	})

	t.Run("unchanged schedule preserves runtime state", func(t *testing.T) {
		fixture.setCurrent(t, fixture.currentID)
		secretID, versionID := uuid.NewV7(), uuid.NewV7()
		if _, err := db.New(fixture.pool).CreateSecret(t.Context(), db.CreateSecretParams{
			ID: pgvalue.UUID(secretID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
			Name: "REPORT_TOKEN", VersionID: pgvalue.UUID(versionID),
			Nonce: make([]byte, 12), Ciphertext: make([]byte, 16),
		}); err != nil {
			t.Fatal(err)
		}
		first := fixture.promote(t, fixture.scheduledID, principal, "", "")
		if first.Code != http.StatusOK {
			t.Fatalf("first promotion status=%d body=%s", first.Code, first.Body.String())
		}
		lastFire := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
		claimExpires := lastFire.Add(time.Minute)
		retryAfter := lastFire.Add(2 * time.Minute)
		dbtest.MustExec(t, t.Context(), fixture.pool, `
			UPDATE schedules
			   SET generation = 7,
			       state_version = 9,
			       last_fire_at = $1,
			       claimed_by = 'worker-test',
			       claim_expires_at = $2,
			       retry_step = 2,
			       retry_after = $3
			 WHERE environment_id = $4
			   AND task_declared_id = 'daily-report'
		`, lastFire, claimExpires, retryAfter, fixture.environmentID)
		second := fixture.promote(t, fixture.scheduledID, principal, "", "")
		if second.Code != http.StatusOK {
			t.Fatalf("second promotion status=%d body=%s", second.Code, second.Body.String())
		}
		var unchanged bool
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT schedules.generation = 7
			   AND schedules.state_version = 9
			   AND schedules.state = 'active'
			   AND schedules.last_fire_at = $1
			   AND schedules.claimed_by = 'worker-test'
			   AND schedules.claim_expires_at = $2
			   AND schedules.retry_step = 2
			   AND schedules.retry_after = $3
			  FROM schedules
			  JOIN schedule_secrets
			    ON schedule_secrets.environment_id = schedules.environment_id
			   AND schedule_secrets.schedule_id = schedules.id
			 WHERE schedules.environment_id = $4
			   AND schedules.task_declared_id = 'daily-report'
		`, lastFire, claimExpires, retryAfter, fixture.environmentID).Scan(&unchanged); err != nil {
			t.Fatal(err)
		}
		if !unchanged {
			t.Fatal("unchanged promotion reset schedule or Secret placement state")
		}
	})

	t.Run("revoked secret leaves committed schedule intact", func(t *testing.T) {
		fixture.setCurrent(t, fixture.scheduledID)
		dbtest.MustExec(t, t.Context(), fixture.pool, `
			UPDATE secrets
			   SET state = 'revoked',
			       state_version = state_version + 1,
			       current_version_id = NULL,
			       revocation_generation = revocation_generation + 1,
			       revoked_at = now(),
			       updated_at = now()
			 WHERE environment_id = $1
			   AND name = 'REPORT_TOKEN'
		`, fixture.environmentID)
		var before string
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT row_to_json(snapshot)::text
			  FROM (
			      SELECT schedules.*, array_agg(schedule_secrets.* ORDER BY schedule_secrets.placement_kind, schedule_secrets.placement_target) AS placements
			        FROM schedules
			        LEFT JOIN schedule_secrets
			          ON schedule_secrets.environment_id = schedules.environment_id
			         AND schedule_secrets.schedule_id = schedules.id
			       WHERE schedules.environment_id = $1
			       GROUP BY schedules.id
			  ) AS snapshot
		`, fixture.environmentID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		recorder := fixture.promote(t, fixture.scheduledID, principal, "", "")
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "REPORT_TOKEN") {
			t.Fatalf("revoked Secret status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if got := fixture.currentDeployment(t); got != fixture.scheduledID {
			t.Fatalf("current after revoked Secret = %s, want %s", got, fixture.scheduledID)
		}
		var after string
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT row_to_json(snapshot)::text
			  FROM (
			      SELECT schedules.*, array_agg(schedule_secrets.* ORDER BY schedule_secrets.placement_kind, schedule_secrets.placement_target) AS placements
			        FROM schedules
			        LEFT JOIN schedule_secrets
			          ON schedule_secrets.environment_id = schedules.environment_id
			         AND schedule_secrets.schedule_id = schedules.id
			       WHERE schedules.environment_id = $1
			       GROUP BY schedules.id
			  ) AS snapshot
		`, fixture.environmentID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatal("failed promotion changed the committed schedule snapshot")
		}
	})

	t.Run("older immutable deployment", func(t *testing.T) {
		fixture.setCurrent(t, fixture.currentID)
		recorder := fixture.promote(t, fixture.olderID, principal, "", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response api.DeploymentResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.ID != fixture.olderID.String() {
			t.Fatalf("promoted = %+v", response)
		}
		if got := fixture.currentDeployment(t); got != fixture.olderID {
			t.Fatalf("current = %s, want older %s", got, fixture.olderID)
		}
		var events int
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT count(*)
			  FROM telemetry_outbox
			 WHERE deployment_id = $1
			   AND kind = 'deployment.promoted'
		`, fixture.olderID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if events < 1 {
			t.Fatalf("promoted events = %d, want at least 1", events)
		}
	})

	t.Run("session route", func(t *testing.T) {
		fixture.setCurrent(t, fixture.currentID)
		session := auth.Actor{
			OrgID: fixture.orgID, UserID: uuid.NewV7(),
			Kind: auth.ActorKindSession, Role: auth.RoleDeveloper,
		}
		recorder := fixture.promote(
			t, fixture.olderID, session,
			fixture.projectID.String(), fixture.environmentID.String(),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("session route status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if got := fixture.currentDeployment(t); got != fixture.olderID {
			t.Fatalf("current after session route = %s, want %s", got, fixture.olderID)
		}
	})

	t.Run("environment row serialization", func(t *testing.T) {
		fixture.setCurrent(t, fixture.currentID)
		lock, err := fixture.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(context.Background())
		if _, err := lock.Exec(t.Context(), `
			SELECT id FROM environments WHERE id = $1 FOR NO KEY UPDATE
		`, fixture.environmentID); err != nil {
			t.Fatal(err)
		}

		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- fixture.promote(t, fixture.olderID, principal, "", "")
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var blocked bool
			if err := fixture.pool.QueryRow(t.Context(), `
				SELECT EXISTS (
					SELECT 1
					  FROM pg_stat_activity
					 WHERE query LIKE '%LockDeploymentPromotionTarget%'
					   AND wait_event_type = 'Lock'
				)
			`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("promotion did not wait for the environment row lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got := fixture.currentDeployment(t); got != fixture.currentID {
			t.Fatalf("current while blocked = %s, want %s", got, fixture.currentID)
		}
		if err := lock.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case recorder := <-done:
			if recorder.Code != http.StatusOK {
				t.Fatalf("serialized promotion status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("promotion did not complete after environment lock release")
		}
		if got := fixture.currentDeployment(t); got != fixture.olderID {
			t.Fatalf("current after serialized promotion = %s, want %s", got, fixture.olderID)
		}
	})
}

func TestPromoteDeploymentPostgresMaximumBulkBudget(t *testing.T) {
	if os.Getenv("HELMR_TEST_DEPLOYMENT_PROMOTION_SCALE") != "1" {
		t.Skip("HELMR_TEST_DEPLOYMENT_PROMOTION_SCALE is not set")
	}
	fixture := newDeploymentPromotionPostgresFixture(t)
	scheduleCount, placementCount := prepareDeploymentPromotionScaleFixture(t, fixture)
	principal := fixture.apiKeyPrincipal()
	beginner := &deploymentPromotionCountingBeginner{pool: fixture.pool}
	fixture.server.tx = beginner

	var walBefore string
	var tempFilesBefore, tempBytesBefore int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT pg_current_wal_insert_lsn()::text, temp_files, temp_bytes
		  FROM pg_stat_database
		 WHERE datname = current_database()
	`).Scan(&walBefore, &tempFilesBefore, &tempBytesBefore); err != nil {
		t.Fatal(err)
	}
	baseline, stopHeapSampling := startDeploymentFinalizeHeapSampling()
	started := time.Now()
	recorder := fixture.promote(t, fixture.scheduledID, principal, "", "")
	elapsed := time.Since(started)
	heapDelta := stopHeapSampling()
	if recorder.Code != http.StatusOK {
		t.Fatalf("maximum promotion status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var walBytes, tempFilesAfter, tempBytesAfter int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint,
		       temp_files,
		       temp_bytes
		  FROM pg_stat_database
		 WHERE datname = current_database()
	`, walBefore).Scan(&walBytes, &tempFilesAfter, &tempBytesAfter); err != nil {
		t.Fatal(err)
	}
	storedSchedules, storedPlacements := fixture.promotionScaleCardinalities(t)
	t.Logf(
		"deployment promotion bulk budget: schedules=%d placements=%d elapsed=%s tx=%s statements=%d schedule_bulk=%d secret_bulk=%d payload_bytes=%d heap_baseline=%d heap_delta=%d wal_bytes=%d temp_files=%d temp_bytes=%d",
		storedSchedules, storedPlacements, elapsed, beginner.transactionDuration(),
		beginner.statements.Load(), beginner.scheduleBulk.Load(), beginner.secretBulk.Load(),
		beginner.payloadBytes.Load(), baseline, heapDelta, walBytes,
		tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore,
	)
	if storedSchedules != scheduleCount || storedPlacements != placementCount {
		t.Fatalf(
			"stored schedules/placements = %d/%d, want %d/%d",
			storedSchedules, storedPlacements, scheduleCount, placementCount,
		)
	}
	if statements := beginner.statements.Load(); statements > 9 {
		t.Fatalf("transaction statements = %d, budget <= 9", statements)
	}
	if beginner.scheduleBulk.Load() != 1 || beginner.secretBulk.Load() != 2 {
		t.Fatalf(
			"bulk schedule/Secret statements = %d/%d, want 1/2",
			beginner.scheduleBulk.Load(), beginner.secretBulk.Load(),
		)
	}
	if elapsed > 65*time.Second {
		t.Fatalf("elapsed/lock time = %s, budget <= 65s", elapsed)
	}
	if heapDelta > 512<<20 {
		t.Fatalf("additional sampled heap = %d, budget <= %d", heapDelta, 512<<20)
	}
	if payload := beginner.payloadBytes.Load(); payload > 268_728_386 {
		t.Fatalf("statement/payload bytes = %d, budget <= 268728386", payload)
	}
	if walBytes > 304<<20 {
		t.Fatalf("WAL bytes = %d, budget <= %d", walBytes, 304<<20)
	}
	if tempBytesAfter-tempBytesBefore > 128<<20 {
		t.Fatalf("temporary bytes = %d, budget <= %d", tempBytesAfter-tempBytesBefore, 128<<20)
	}
	bulkStarted := make(chan struct{}, 1)
	cancelBeginner := &deploymentPromotionCountingBeginner{
		pool: fixture.pool, secretBulkStarted: bulkStarted,
	}
	fixture.server.tx = cancelBeginner
	blocker, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(t.Context(), `
		SELECT schedule_id
		  FROM schedule_secrets
		 WHERE environment_id = $1
		 ORDER BY schedule_id, placement_kind, placement_target
		 LIMIT 1
		 FOR UPDATE
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- fixture.promoteContext(ctx, t, fixture.scheduledID, principal, "", "")
	}()
	select {
	case <-bulkStarted:
	case completed := <-done:
		t.Fatalf(
			"promotion completed before bulk Secret replacement: status=%d body=%s",
			completed.Code, completed.Body.String(),
		)
	case <-time.After(5 * time.Second):
		var query, waitType, waitEvent string
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT query, coalesce(wait_event_type, ''), coalesce(wait_event, '')
			  FROM pg_stat_activity
			 WHERE pid = $1
		`, cancelBeginner.backendPID.Load()).Scan(&query, &waitType, &waitEvent); err != nil {
			t.Fatalf("bulk Secret replacement did not start; inspect activity: %v", err)
		}
		t.Fatalf(
			"bulk Secret replacement did not start: wait=%s/%s query=%s",
			waitType, waitEvent, query,
		)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				  FROM pg_stat_activity
				 WHERE pid = $1
				   AND wait_event_type = 'Lock'
				   AND position('DeleteScheduleSecretsForSchedules' IN query) > 0
			)
		`, cancelBeginner.backendPID.Load()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bulk Secret replacement did not reach the PostgreSQL lock wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	canceledAt := time.Now()
	cancel()
	select {
	case canceled := <-done:
		if canceled.Code == http.StatusOK {
			t.Fatal("canceled maximum promotion succeeded")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("canceled promotion exceeded 50ms rollback budget")
	}
	rollbackElapsed := time.Since(canceledAt)
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	storedSchedules, storedPlacements = fixture.promotionScaleCardinalities(t)
	if got := fixture.currentDeployment(t); got != fixture.scheduledID ||
		storedSchedules != scheduleCount || storedPlacements != placementCount {
		t.Fatalf(
			"state after cancellation: current=%s schedules=%d placements=%d",
			got, storedSchedules, storedPlacements,
		)
	}
	var invalidSchedules, invalidPlacements int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*)
		          FROM schedules
		         WHERE environment_id = $1
		           AND (deployment_id <> $2 OR generation <> 1 OR state_version <> 1 OR state <> 'active')),
		       (SELECT count(*)
		          FROM schedule_secrets
		          JOIN secrets ON secrets.environment_id = schedule_secrets.environment_id
		                      AND secrets.id = schedule_secrets.secret_id
		         WHERE schedule_secrets.environment_id = $1
		           AND (schedule_secrets.placement_kind <> 'env'
		                OR schedule_secrets.placement_target <> secrets.name))
	`, fixture.environmentID, fixture.scheduledID).Scan(&invalidSchedules, &invalidPlacements); err != nil {
		t.Fatal(err)
	}
	if invalidSchedules != 0 || invalidPlacements != 0 {
		t.Fatalf("mixed state after cancellation: schedules=%d placements=%d", invalidSchedules, invalidPlacements)
	}
	t.Logf("deployment promotion bulk cancellation rollback: %s", rollbackElapsed)
}

func TestPromoteDeploymentPostgresAvoidsScheduleAdmissionSecretLockInversion(t *testing.T) {
	fixture := newDeploymentPromotionPostgresFixture(t)
	principal := fixture.apiKeyPrincipal()
	secretID, versionID := uuid.NewV7(), uuid.NewV7()
	if _, err := db.New(fixture.pool).CreateSecret(t.Context(), db.CreateSecretParams{
		ID: pgvalue.UUID(secretID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
		Name: "REPORT_TOKEN", VersionID: pgvalue.UUID(versionID),
		Nonce: make([]byte, 12), Ciphertext: make([]byte, 16),
	}); err != nil {
		t.Fatal(err)
	}
	first := fixture.promote(t, fixture.scheduledID, principal, "", "")
	if first.Code != http.StatusOK {
		t.Fatalf("initial promotion status=%d body=%s", first.Code, first.Body.String())
	}

	admission, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admission.Rollback(context.Background()) })
	if _, err := admission.Exec(t.Context(), `
		SELECT id
		  FROM schedules
		 WHERE environment_id = $1
		   AND task_declared_id = 'daily-report'
		 FOR UPDATE
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}

	beginner := &deploymentPromotionCountingBeginner{pool: fixture.pool}
	fixture.server.tx = beginner
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- fixture.promoteContext(
			t.Context(), t, fixture.scheduledID, principal, "", "",
		)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if pid := beginner.backendPID.Load(); pid != 0 {
			if err := fixture.pool.QueryRow(t.Context(), `
				SELECT EXISTS (
					SELECT 1
					  FROM pg_stat_activity
					 WHERE pid = $1
					   AND wait_event_type = 'Lock'
					   AND position('ReconcileSchedules' IN query) > 0
				)
			`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("promotion did not wait on the admission-held schedule")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := admission.Exec(t.Context(), `SET LOCAL lock_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}
	// Workspace Secret insertion takes this FK key-share lock after admission
	// locks the schedule. It must coexist with promotion's Secret lock.
	if _, err := admission.Exec(t.Context(), `
		SELECT id
		  FROM secrets
		 WHERE environment_id = $1
		   AND name = 'REPORT_TOKEN'
		 FOR KEY SHARE
	`, fixture.environmentID); err != nil {
		t.Fatalf("schedule admission Secret reference blocked by promotion: %v", err)
	}
	if err := admission.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case promoted := <-done:
		if promoted.Code != http.StatusOK {
			t.Fatalf("promotion status=%d body=%s", promoted.Code, promoted.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("promotion did not resume after schedule admission committed")
	}
}

func prepareDeploymentPromotionScaleFixture(
	t *testing.T,
	fixture deploymentPromotionPostgresFixture,
) (int, int) {
	t.Helper()
	const scheduleCount = 9_999
	placements := make([]api.WorkspaceSecret, workspace.MaxSecretPlacements)
	queries := db.New(fixture.pool)
	for index := range placements {
		name := fmt.Sprintf("SECRET_%02d", index)
		placements[index] = api.WorkspaceSecret{Name: name, Env: name}
		secretID, versionID := uuid.NewV7(), uuid.NewV7()
		if _, err := queries.CreateSecret(t.Context(), db.CreateSecretParams{
			ID: pgvalue.UUID(secretID), EnvironmentID: pgvalue.UUID(fixture.environmentID),
			Name: name, VersionID: pgvalue.UUID(versionID),
			Nonce: make([]byte, 12), Ciphertext: make([]byte, 16),
		}); err != nil {
			t.Fatal(err)
		}
	}
	taskManifest, taskDigest, err := deployment.CanonicalManifestAndDigest(promotionScaleJSON(t, deployment.TaskManifest{
		Payload: deployment.SchemaManifest{Kind: deployment.SchemaKindStandard},
		Run: deployment.RunManifest{
			Queue: "default", MaxDurationMs: 300_000,
			Retry: deployment.RetryManifest{Enabled: false},
		},
		Schedule: &deployment.ScheduleManifest{
			Cron: "0 9 * * *", Timezone: "UTC",
			Workspace: deployment.ScheduleWorkspaceManifest{
				SandboxDeclaredID: "reporting", Secrets: placements,
			},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var imageID pgtype.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT id FROM artifacts
		 WHERE environment_id = $1 AND kind = 'workspace_image'
		 LIMIT 1
	`, fixture.environmentID).Scan(&imageID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		DELETE FROM deployment_definitions WHERE deployment_id = $1
	`, fixture.scheduledID)
	definitionCount := scheduleCount + 1
	params := db.CreateDeploymentDefinitionsParams{
		EnvironmentID:   pgvalue.UUID(fixture.environmentID),
		DeploymentID:    pgvalue.UUID(fixture.scheduledID),
		ManifestVersion: deployment.DeploymentPlanFormatVersion,
		Ids:             make([]pgtype.UUID, definitionCount), Kinds: make([]string, definitionCount),
		DeclaredIds: make([]string, definitionCount), Manifests: make([][]byte, definitionCount),
		ManifestDigests: make([][]byte, definitionCount), ArtifactIds: make([]pgtype.UUID, definitionCount),
	}
	params.Ids[0] = pgvalue.UUID(uuid.NewV7())
	params.Kinds[0] = string(deployment.DefinitionKindSandbox)
	params.DeclaredIds[0] = "reporting"
	params.Manifests[0] = []byte(`{}`)
	params.ManifestDigests[0] = make([]byte, 32)
	params.ArtifactIds[0] = imageID
	for index := 1; index < definitionCount; index++ {
		params.Ids[index] = pgvalue.UUID(uuid.NewV7())
		params.Kinds[index] = string(deployment.DefinitionKindTask)
		params.DeclaredIds[index] = fmt.Sprintf("task-%05d", index-1)
		params.Manifests[index] = taskManifest
		params.ManifestDigests[index] = taskDigest[:]
	}
	inserted, err := queries.CreateDeploymentDefinitions(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != int64(definitionCount) {
		t.Fatalf("inserted definitions = %d, want %d", inserted, definitionCount)
	}
	return scheduleCount, scheduleCount * len(placements)
}

func promotionScaleJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func (fixture deploymentPromotionPostgresFixture) promotionScaleCardinalities(t *testing.T) (int, int) {
	t.Helper()
	var schedules, placements int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM schedules WHERE environment_id = $1),
		       (SELECT count(*) FROM schedule_secrets WHERE environment_id = $1)
	`, fixture.environmentID).Scan(&schedules, &placements); err != nil {
		t.Fatal(err)
	}
	return schedules, placements
}

type deploymentPromotionCountingBeginner struct {
	pool              *pgxpool.Pool
	statements        atomic.Int64
	scheduleBulk      atomic.Int64
	secretBulk        atomic.Int64
	payloadBytes      atomic.Int64
	transactionNanos  atomic.Int64
	backendPID        atomic.Int64
	secretBulkStarted chan struct{}
}

func (b *deploymentPromotionCountingBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	b.backendPID.Store(int64(tx.Conn().PgConn().PID()))
	return &deploymentPromotionCountingTx{Tx: tx, owner: b, started: time.Now()}, nil
}

func (b *deploymentPromotionCountingBeginner) transactionDuration() time.Duration {
	return time.Duration(b.transactionNanos.Load())
}

type deploymentPromotionCountingTx struct {
	pgx.Tx
	owner   *deploymentPromotionCountingBeginner
	started time.Time
}

func (tx *deploymentPromotionCountingTx) Exec(
	ctx context.Context, sql string, args ...any,
) (pgconn.CommandTag, error) {
	tx.record(sql, args)
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *deploymentPromotionCountingTx) Query(
	ctx context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	tx.record(sql, args)
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *deploymentPromotionCountingTx) QueryRow(
	ctx context.Context, sql string, args ...any,
) pgx.Row {
	tx.record(sql, args)
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *deploymentPromotionCountingTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	tx.owner.transactionNanos.Store(int64(time.Since(tx.started)))
	return err
}

func (tx *deploymentPromotionCountingTx) record(sql string, args []any) {
	tx.owner.statements.Add(1)
	tx.owner.payloadBytes.Add(int64(len(sql)) + deploymentPromotionArgumentBytes(args))
	if strings.Contains(sql, "-- name: ReconcileSchedules") {
		tx.owner.scheduleBulk.Add(1)
	}
	if strings.Contains(sql, "-- name: DeleteScheduleSecretsForSchedules") ||
		strings.Contains(sql, "-- name: InsertScheduleSecrets") {
		tx.owner.secretBulk.Add(1)
		if strings.Contains(sql, "-- name: DeleteScheduleSecretsForSchedules") && tx.owner.secretBulkStarted != nil {
			select {
			case tx.owner.secretBulkStarted <- struct{}{}:
			default:
			}
		}
	}
}

func deploymentPromotionArgumentBytes(args []any) int64 {
	var total int64
	for _, arg := range args {
		switch value := arg.(type) {
		case []pgtype.UUID:
			total += int64(len(value) * 16)
		case []pgtype.Timestamptz:
			total += int64(len(value) * 8)
		case []string:
			for _, item := range value {
				total += int64(len(item))
			}
		case []byte:
			total += int64(len(value))
		case pgtype.UUID:
			total += 16
		case int32:
			total += 4
		case int64:
			total += 8
		case string:
			total += int64(len(value))
		}
	}
	return total
}

var _ TxBeginner = (*deploymentPromotionCountingBeginner)(nil)

func (fixture deploymentPromotionPostgresFixture) apiKeyPrincipal() auth.Actor {
	return auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionTasksDeploy},
	}
}

func (fixture deploymentPromotionPostgresFixture) setCurrent(t *testing.T, id uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), fixture.pool, `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, id, fixture.environmentID)
}

func (fixture deploymentPromotionPostgresFixture) currentDeployment(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT current_deployment_id FROM environments WHERE id = $1
	`, fixture.environmentID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (fixture deploymentPromotionPostgresFixture) promote(
	t *testing.T,
	deploymentID uuid.UUID,
	principal auth.Actor,
	projectID string,
	environmentID string,
) *httptest.ResponseRecorder {
	t.Helper()
	return fixture.promoteContext(
		t.Context(), t, deploymentID, principal, projectID, environmentID,
	)
}

func (fixture deploymentPromotionPostgresFixture) promoteContext(
	ctx context.Context,
	t *testing.T,
	deploymentID uuid.UUID,
	principal auth.Actor,
	projectID string,
	environmentID string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	route := chi.NewRouteContext()
	route.URLParams.Add("deploymentID", deploymentID.String())
	if projectID != "" {
		route.URLParams.Add("projectID", projectID)
	}
	if environmentID != "" {
		route.URLParams.Add("environmentID", environmentID)
	}
	ctx = context.WithValue(ctx, chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	recorder := httptest.NewRecorder()
	fixture.server.promoteDeployment(recorder, request.WithContext(ctx))
	return recorder
}

func newDeploymentPromotionPostgresFixture(t *testing.T) deploymentPromotionPostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	pool := database.Pool
	fixture := deploymentPromotionPostgresFixture{
		pool:          pool,
		orgID:         uuid.NewV7(),
		projectID:     uuid.NewV7(),
		environmentID: uuid.NewV7(),
		otherEnvID:    uuid.NewV7(),
		olderID:       uuid.NewV7(),
		currentID:     uuid.NewV7(),
		scheduledID:   uuid.NewV7(),
		otherID:       uuid.NewV7(),
		server: &Server{
			db:  db.New(pool),
			tx:  pool,
			log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	programID, imageID := uuid.NewV7(), uuid.NewV7()
	otherProgramID := uuid.NewV7()
	digests := []string{
		sha256Digest(1), sha256Digest(2), sha256Digest(3), sha256Digest(4),
		sha256Digest(5), sha256Digest(6), sha256Digest(7),
	}
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO regions (id, display_name) VALUES ('us-east-1', 'Promotion Test')
	`)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO organizations (id, name, slug) VALUES ($1, 'Promotion Test', $2)
	`, fixture.orgID, "promotion-"+fixture.orgID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, 'us-east-1', $3, 'Promotion Test')
	`, fixture.projectID, fixture.orgID, "promotion-"+fixture.projectID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Staging', '#3366ff'),
		       ($5, $2, $3, $6, 'Preview', '#112233')
	`, fixture.environmentID, fixture.orgID, fixture.projectID,
		"staging-"+fixture.environmentID.String(),
		fixture.otherEnvID, "preview-"+fixture.otherEnvID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $3, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $4, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $5, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $6, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
		       ($1, $7, 1, 'application/octet-stream'),
		       ($1, $8, 1, 'application/vnd.helmr.runtime.v0+squashfs')
	`, fixture.orgID, digests[0], digests[1], digests[2], digests[3],
		digests[4], digests[5], digests[6])
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $4, $5, $6, $7, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
		       ($2, $4, $5, $6, $8, 'workspace_image', 1, 'application/octet-stream'),
		       ($3, $4, $5, $9, $7, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs')
	`, programID, imageID, otherProgramID, fixture.orgID, fixture.projectID,
		fixture.environmentID, digests[4], digests[5], fixture.otherEnvID)
	queueConfig := []byte(`{"formatVersion":0,"queues":[{"name":"default"}]}`)
	insertPromotionDeployment(t, pool, fixture.olderID, fixture.orgID, fixture.projectID,
		fixture.environmentID, "older", digests[0], digests[6], programID, queueConfig)
	insertPromotionDeployment(t, pool, fixture.currentID, fixture.orgID, fixture.projectID,
		fixture.environmentID, "current", digests[1], digests[6], programID, queueConfig)
	insertPromotionDeployment(t, pool, fixture.scheduledID, fixture.orgID, fixture.projectID,
		fixture.environmentID, "scheduled", digests[2], digests[6], programID, queueConfig)
	insertPromotionDeployment(t, pool, fixture.otherID, fixture.orgID, fixture.projectID,
		fixture.otherEnvID, "other", digests[3], digests[6], otherProgramID, queueConfig)

	scheduledManifest := []byte(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * *","timezone":"UTC","workspace":{"sandboxId":"reporting","secrets":[{"env":"REPORT_TOKEN","name":"REPORT_TOKEN"}]}}}`,
	)
	canonical, digest, err := deployment.CanonicalManifestAndDigest(scheduledManifest)
	if err != nil {
		t.Fatal(err)
	}
	sandboxID, taskID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployment_definitions (
		    id, environment_id, deployment_id, kind, declared_id,
		    manifest_version, manifest, manifest_digest, artifact_id
		) VALUES
		    ($1, $3, $4, 'sandbox', 'reporting', 0, '{}'::jsonb, decode(repeat('04', 32), 'hex'), $5),
		    ($2, $3, $4, 'task', 'daily-report', 0, $6::jsonb, $7, NULL)
	`, sandboxID, taskID, fixture.environmentID, fixture.scheduledID, imageID,
		canonical, digest[:])
	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, fixture.currentID, fixture.environmentID)
	return fixture
}

func insertPromotionDeployment(
	t *testing.T,
	pool *pgxpool.Pool,
	id uuid.UUID,
	orgID uuid.UUID,
	projectID uuid.UUID,
	environmentID uuid.UUID,
	version string,
	bundleDigest string,
	runtimeDigest string,
	programID uuid.UUID,
	queueConfig []byte,
) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployments (
		    id, org_id, project_id, environment_id, version, bundle_digest,
		    runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		) VALUES (
		    $1, $2, $3, $4, $5, $6, $7, $8, decode(repeat('03', 32), 'hex'), $9::jsonb
		)
	`, id, orgID, projectID, environmentID, version, bundleDigest, runtimeDigest, programID, queueConfig)
}

func sha256Digest(n int) string {
	return "sha256:" + fmt.Sprintf("%064x", n)
}
