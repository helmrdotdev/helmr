package db_test

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestCapacityWorkerListingProjectsCurrentRowPerOpaqueLocator(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	queries := db.New(pool)
	resourceID := "operator-host-1"
	oldID := uuid.NewV7()
	currentID := uuid.NewV7()
	oldServiceID := uuid.NewV7()

	dbtest.MustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, worker_pool_id, state,
			current_epoch, current_service_id, epoch_started_at,
			draining_at, termination_ready_at, created_at, updated_at
		) VALUES ($1, $3, $4, $6, 'termination_ready',
		          1, $5, now() - interval '1 hour',
		          now() - interval '1 hour', now() - interval '1 hour',
		          now() - interval '1 hour', now() - interval '1 hour'),
		         ($2, $3, $4, $6, 'registering',
		          NULL, NULL, NULL, NULL, NULL, now(), now())
	`, oldID, currentID, resourceID, dbtest.DefaultWorkerGroupID, oldServiceID,
		dbtest.DefaultWorkerPoolID)

	rows, err := queries.ListCapacityWorkerInstances(ctx, db.ListCapacityWorkerInstancesParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ResourceIds:   []string{resourceID},
		States:        []string{},
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID.Bytes != currentID || rows[0].State != string(db.WorkerInstanceStateRegistering) {
		t.Fatalf("rows = %#v, want only current registering row", rows)
	}

	rows, err = queries.ListCapacityWorkerInstances(ctx, db.ListCapacityWorkerInstancesParams{
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ResourceIds:   []string{resourceID},
		States:        []string{string(db.WorkerInstanceStateTerminationReady)},
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("filtered rows = %#v, stale termination receipt was revived", rows)
	}
}
