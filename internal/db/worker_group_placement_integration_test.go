package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestEnvironmentRecordPlacementSurvivesStaleWorkerGroupHealth(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	_ = seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}

	placement, err := queries.GetWorkerGroupPlacementForRecord(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if placement.WorkerGroupID != dbtest.DefaultWorkerGroupID {
		t.Fatalf("placement worker group = %q", placement.WorkerGroupID)
	}
}

func TestWorkerGroupHealthControlsReadiness(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	_ = seedIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET health_state = 'unavailable',
		       routing_fresh_until = now() + interval '1 minute'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}

	readiness, err := queries.GetControlWorkerGroupReadiness(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Routable.Bool {
		t.Fatalf("routable = true with unavailable worker group")
	}

	if err := workergroup.ReportHealth(ctx, queries, workergroup.HealthConfig{
		WorkerGroupID:      dbtest.DefaultWorkerGroupID,
		Component:          "dispatcher",
		RequiredComponents: workergroup.RoutingRequiredComponents(),
	}); err != nil {
		t.Fatal(err)
	}
	readiness, err = queries.GetControlWorkerGroupReadiness(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.Routable.Bool {
		t.Fatalf("routable = false after worker group health reported: %+v", readiness)
	}
}

func TestPrepareQueuedRunDispatchRequiresFreshWorkerGroupHealth(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedIntegration(t, ctx, pool)
	queries := db.New(pool)
	seedSessionForRun(t, ctx, pool, ids)
	runtimeID := "runtime-routing-health"
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if err := queries.EnsureRuntimeReleaseSelection(ctx, runtimeID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET routing_fresh_until = now() - interval '1 minute'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.PrepareQueuedRunDispatch(ctx, db.PrepareQueuedRunDispatchParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
}
