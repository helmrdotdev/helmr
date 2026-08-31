package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	request := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	route := chi.NewRouteContext()
	route.URLParams.Add("deploymentID", deploymentID.String())
	if projectID != "" {
		route.URLParams.Add("projectID", projectID)
	}
	if environmentID != "" {
		route.URLParams.Add("environmentID", environmentID)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
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
