package db_test

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestWorkerDemandRemainsVisibleWithoutEligibleWorkers(t *testing.T) {
	f, _ := newDeploymentBuildFixture(t)
	mustExec(t, f.ctx, f.pool, `DELETE FROM worker_instances WHERE id = $1`, f.workerID)

	rows, err := f.queries.ListWorkerDemandObservations(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	row := demandObservationForGroup(t, rows, f.groupID)
	if row.QueuedBuilds < 1 {
		t.Fatalf("queued builds = %d, want at least 1", row.QueuedBuilds)
	}
	if row.ReadyBuildWorkers != 0 {
		t.Fatalf("ready Build workers = %d, want 0", row.ReadyBuildWorkers)
	}
	if !row.ObservedAt.Valid {
		t.Fatal("observation timestamp is missing")
	}

	mustExec(t, f.ctx, f.pool, `UPDATE worker_groups SET state = 'paused' WHERE id = $1`, f.groupID)
	rows, err = f.queries.ListWorkerDemandObservations(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	row = demandObservationForGroup(t, rows, f.groupID)
	if row.QueuedRuns != 0 || row.QueuedBuilds != 0 || row.ReadyRunWorkers != 0 || row.ReadyBuildWorkers != 0 {
		t.Fatalf("paused group owns demand or eligible capacity: %#v", row)
	}
}

func demandObservationForGroup(t *testing.T, rows []db.ListWorkerDemandObservationsRow, groupID string) db.ListWorkerDemandObservationsRow {
	t.Helper()
	for _, row := range rows {
		if row.WorkerGroupID == groupID {
			return row
		}
	}
	t.Fatalf("missing demand observation for Worker group %q", groupID)
	return db.ListWorkerDemandObservationsRow{}
}
