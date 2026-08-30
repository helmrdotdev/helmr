package run

import (
	"context"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresFixture struct {
	base          runtest.Fixture
	pool          *pgxpool.Pool
	queries       *db.Queries
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	workerID      uuid.UUID
}

type leasedRun struct {
	leaseID uuid.UUID
	runID   uuid.UUID
}

func newPostgresFixture(t *testing.T) postgresFixture {
	t.Helper()
	base := runtest.New(t)
	return postgresFixture{
		base:          base,
		pool:          base.Pool,
		queries:       db.New(base.Pool),
		orgID:         base.OrgID,
		projectID:     base.ProjectID,
		environmentID: base.EnvironmentID,
		workerID:      base.WorkerID,
	}
}

func (fixture postgresFixture) addRun(
	t *testing.T,
	state string,
	assignedAt time.Time,
) leasedRun {
	t.Helper()
	work := fixture.base.AddRunLease(t, state, assignedAt)
	return leasedRun{leaseID: work.LeaseID, runID: work.RunID}
}

func (fixture postgresFixture) convertToActor(
	t *testing.T,
	ctx context.Context,
	work leasedRun,
	retryPolicy string,
) uuid.UUID {
	t.Helper()
	return fixture.base.ConvertToActor(
		t,
		ctx,
		runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
		retryPolicy,
	)
}
