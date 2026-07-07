package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateScopedRunUsesExistingSessionWorkspacePlacement(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	seedDefaultPlacementWorker(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ('us-west-2', 'aws', 'us-west-2', 'US West (Oregon)')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE projects
		   SET default_region_id = 'us-west-2'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.projectID); err != nil {
		t.Fatal(err)
	}
	markDefaultWorkerGroupDrainingWithStaleHealth(t, ctx, pool, ids)

	run, err := queries.CreateScopedRun(ctx, db.CreateScopedRunParams{
		AllowDrainingRoute:    true,
		OrgID:                 pgvalue.UUID(ids.orgID),
		ProjectID:             pgvalue.UUID(ids.projectID),
		EnvironmentID:         pgvalue.UUID(ids.environmentID),
		SessionID:             pgvalue.UUID(sessionID),
		WorkspaceID:           pgvalue.UUID(ids.workspaceID),
		ID:                    pgvalue.UUID(uuid.Must(uuid.NewV7())),
		PublicID:              testPublicID(t, publicid.Run),
		DeploymentID:          pgvalue.UUID(ids.deploymentID),
		DeploymentTaskID:      pgvalue.UUID(ids.taskID),
		DeploymentVersion:     "v2",
		ApiVersion:            "2026-06-06",
		SdkVersion:            "test-sdk",
		CliVersion:            "test-cli",
		TaskID:                "approval-task",
		Payload:               []byte(`{"source":"existing-placement"}`),
		Metadata:              []byte(`{}`),
		Tags:                  []string{},
		LockedRetryPolicy:     []byte(`{"enabled":false}`),
		QueueName:             "default",
		QueueConcurrencyLimit: pgtype.Int4{},
		ConcurrencyKey:        pgtype.Text{},
		Priority:              0,
		QueueTimestamp:        pgvalue.Timestamptz(time.Now()),
		Ttl:                   "1h",
		QueuedExpiresAt:       pgtype.Timestamptz{},
		MaxActiveDurationMs:   300000,
		TraceID:               pgtype.Text{String: "11111111111111111111111111111111", Valid: true},
		RootSpanID:            "2222222222222222",
		EventPayload:          []byte(`{"kind":"run.created"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkerGroupID == "" {
		t.Fatal("created run worker_group_id is empty")
	}
}
