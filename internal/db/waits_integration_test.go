package db_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolveImmediateTokenWaitForRunWaitCompletesNewWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	tokenID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		INSERT INTO tokens (
			id, public_id, org_id, project_id, environment_id, state,
			timeout_at, completion_data, completion_fingerprint, completed_at
		)
		VALUES ($1, $5, $2, $3, $4, 'completed', now() + interval '1 hour', '{"ok":true}'::jsonb, 'done', now())
	`, tokenID, ids.orgID, ids.projectID, ids.environmentID, testTokenPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindToken,
		TokenID:          pgvalue.UUID(tokenID),
		ExpiresAt:        timestamptz(time.Now().Add(time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveImmediateTokenWaitForRunWait(ctx, db.ResolveImmediateTokenWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != db.RunWaitStateResuming {
		t.Fatalf("run wait state = %s, want resuming", resolved.State)
	}

	var waitState db.WaitState
	var result []byte
	if err := pool.QueryRow(ctx, `
		SELECT state, result
		  FROM waits
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, waitID).Scan(&waitState, &result); err != nil {
		t.Fatal(err)
	}
	if waitState != db.WaitStateCompleted {
		t.Fatalf("wait state = %s, want completed", waitState)
	}
	var payload map[string]bool
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload["ok"] {
		t.Fatalf("wait result = %s, want completed token payload", string(result))
	}
}

func TestResolveImmediateTokenWaitForRunWaitResumesTerminalWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	tokenID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx, `
		INSERT INTO tokens (
			id, public_id, org_id, project_id, environment_id, state,
			timeout_at, completion_data, completion_fingerprint, completed_at
		)
		VALUES ($1, $5, $2, $3, $4, 'completed', now() + interval '1 hour', '{"ok":true}'::jsonb, 'done', now())
	`, tokenID, ids.orgID, ids.projectID, ids.environmentID, testTokenPublicID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindToken,
		TokenID:          pgvalue.UUID(tokenID),
		ExpiresAt:        timestamptz(time.Now().Add(time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE waits
		   SET state = 'completed',
		       result = '{"ok":true}'::jsonb,
		       completed_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, waitID); err != nil {
		t.Fatal(err)
	}

	resolved, err := queries.ResolveImmediateTokenWaitForRunWait(ctx, db.ResolveImmediateTokenWaitForRunWaitParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		RunWaitID:     pgvalue.UUID(runWaitID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != db.RunWaitStateResuming {
		t.Fatalf("run wait state = %s, want resuming", resolved.State)
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStateCompleted, runWaitID, db.RunWaitStateResuming)
}

func TestExpireDueRunWaitsIsScopedByWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	otherWorkerGroupID := dbtest.DefaultWorkerGroupID + "-expiry-other"

	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_groups (id, region_id, name, state, health_state, routing_fresh_until)
		VALUES ($1, $2, $1, 'active', 'healthy', now() + interval '5 minutes')
	`, otherWorkerGroupID, dbtest.DefaultRegionID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindTimer,
		CompletedAfter:   timestamptz(time.Now().Add(time.Hour)),
		ExpiresAt:        timestamptz(time.Now().Add(-time.Minute)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	wrongGroupRows, err := queries.ExpireDueRunWaits(ctx, db.ExpireDueRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: otherWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrongGroupRows) != 0 {
		t.Fatalf("wrong worker group expired %d waits, want 0", len(wrongGroupRows))
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStatePending, runWaitID, db.RunWaitStateHotWaiting)

	defaultRows, err := queries.ExpireDueRunWaits(ctx, db.ExpireDueRunWaitsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultRows) != 1 {
		t.Fatalf("default worker group expired %d waits, want 1", len(defaultRows))
	}
	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStateExpired, runWaitID, db.RunWaitStateResuming)
}

func TestCancelRunCancelsPendingWaits(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	_, runLeaseID, workerID := seedRunningSessionLease(t, ctx, pool, ids)
	waitID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	operation := seedCancelOperation(t, ctx, queries, ids, "interrupt")

	if _, err := queries.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		WaitID:           pgvalue.UUID(waitID),
		PublicID:         testWaitPublicID(t),
		Kind:             db.WaitKindTimer,
		CompletedAfter:   timestamptz(time.Now().Add(time.Hour)),
		ExpiresAt:        timestamptz(time.Now().Add(2 * time.Hour)),
		RunWaitID:        pgvalue.UUID(runWaitID),
		CheckpointDelay:  interval(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		RunID:         pgvalue.UUID(ids.runID),
		Reason:        "interrupt",
		Force:         false,
		OperationID:   operation.ID,
	}); err != nil {
		t.Fatal(err)
	}

	assertWaitAndRunWaitState(t, ctx, pool, ids.orgID, waitID, db.WaitStateCancelled, runWaitID, db.RunWaitStateCancelled)
}

func assertWaitAndRunWaitState(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, orgID uuid.UUID, waitID uuid.UUID, wantWaitState db.WaitState, runWaitID uuid.UUID, wantRunWaitState db.RunWaitState) {
	t.Helper()
	var waitState db.WaitState
	var runWaitState db.RunWaitState
	if err := pool.QueryRow(ctx, `
		SELECT waits.state, run_waits.state
		  FROM waits
		  JOIN run_waits ON run_waits.org_id = waits.org_id
		                AND run_waits.wait_id = waits.id
		 WHERE waits.org_id = $1
		   AND waits.id = $2
		   AND run_waits.id = $3
	`, orgID, waitID, runWaitID).Scan(&waitState, &runWaitState); err != nil {
		t.Fatal(err)
	}
	if waitState != wantWaitState {
		t.Fatalf("wait state = %s, want %s", waitState, wantWaitState)
	}
	if runWaitState != wantRunWaitState {
		t.Fatalf("run wait state = %s, want %s", runWaitState, wantRunWaitState)
	}
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
