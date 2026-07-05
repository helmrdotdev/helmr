package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCloseSessionRejectsPendingContinuationRequest(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	markCurrentRunTerminal(t, ctx, pool, ids)
	seedPendingSessionRunRequest(t, ctx, pool, ids, sessionID)

	_, err := queries.CloseSession(ctx, db.CloseSessionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(sessionID),
		Reason:        "done",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("close error = %v, want pgx.ErrNoRows", err)
	}

	activity, err := queries.GetSessionActivity(ctx, db.GetSessionActivityParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(sessionID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if activity.Activity != "queued" || activity.CanClose {
		t.Fatalf("activity=%q can_close=%v, want queued/false", activity.Activity, activity.CanClose)
	}
}

func TestExpireDueSessionsExpiresOnlyIdleSessions(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	sessionID := seedSessionForRun(t, ctx, pool, ids)
	seedSessionRun(t, ctx, pool, ids, sessionID)
	if _, err := pool.Exec(ctx, `
		UPDATE sessions
		   SET expires_at = now() - interval '1 minute'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, sessionID); err != nil {
		t.Fatal(err)
	}

	expired, err := queries.ExpireDueSessions(ctx, db.ExpireDueSessionsParams{OrgID: pgvalue.UUID(ids.orgID), WorkerGroupID: dbtest.DefaultWorkerGroupID})
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("expired active sessions = %+v, want none", expired)
	}

	markCurrentRunTerminal(t, ctx, pool, ids)
	expired, err = queries.ExpireDueSessions(ctx, db.ExpireDueSessionsParams{OrgID: pgvalue.UUID(ids.orgID), WorkerGroupID: dbtest.DefaultWorkerGroupID})
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != pgvalue.UUID(sessionID) || expired[0].Status != db.SessionStatusExpired || !expired[0].ExpiredAt.Valid {
		t.Fatalf("expired sessions = %+v", expired)
	}

	again, err := queries.ExpireDueSessions(ctx, db.ExpireDueSessionsParams{OrgID: pgvalue.UUID(ids.orgID), WorkerGroupID: dbtest.DefaultWorkerGroupID})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second sweep expired sessions = %+v, want none", again)
	}
}

func markCurrentRunTerminal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'succeeded',
		       execution_status = 'finished',
		       terminal_outcome = 'succeeded',
		       finished_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
}

func seedPendingSessionRunRequest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids integrationIDs, sessionID uuid.UUID) {
	t.Helper()
	deploymentStreamID := uuid.Must(uuid.NewV7())
	streamID := uuid.Must(uuid.NewV7())
	streamRecordID := uuid.Must(uuid.NewV7())
	requestID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_streams (id, org_id, worker_group_id, project_id, environment_id, deployment_id, name, direction)
		VALUES ($1, $2, $3, $4, $5, $6, 'user.input', 'input')
	`, deploymentStreamID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, ids.deploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO streams (id, public_id, org_id, worker_group_id, project_id, environment_id, session_id, deployment_stream_id, name, direction)
		VALUES ($1, $8, $2, $3, $4, $5, $6, $7, 'user.input', 'input')
	`, streamID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, sessionID, deploymentStreamID, testStreamPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO stream_records (
				id, public_id, org_id, worker_group_id, project_id, environment_id, session_id, stream_id, direction,
				sequence, data, source_type, source_id, created_at
			)
			VALUES ($1, $9, $2, $3, $4, $5, $6, $7, 'input', 1, '{"text":"continue"}', 'api_key', 'test', $8)
		`, streamRecordID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, sessionID, streamID, time.Now().UTC(), testStreamRecordPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
			INSERT INTO session_run_requests (
				id, org_id, worker_group_id, project_id, environment_id, session_id, stream_record_id, stream_id, cause_kind, status
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'stream_record', 'accepted')
		`, requestID, ids.orgID, dbtest.DefaultWorkerGroupID, ids.projectID, ids.environmentID, sessionID, streamRecordID, streamID); err != nil {
		t.Fatal(err)
	}
}
