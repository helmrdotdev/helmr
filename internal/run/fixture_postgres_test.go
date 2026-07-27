package run

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testWorkerGroup    = runtest.WorkerGroup
	testWorkerProtocol = runtest.WorkerProtocol
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

type handoffChain struct {
	outerRunID          uuid.UUID
	parentRunID         uuid.UUID
	outerWaitID         uuid.UUID
	outerCheckpoint     uuid.UUID
	outerResumeID       uuid.UUID
	enclosingWaitID     uuid.UUID
	enclosingCheckpoint uuid.UUID
	enclosingResumeID   uuid.UUID
	runtimeID           uuid.UUID
	mountID             uuid.UUID
	versionID           uuid.UUID
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

func (fixture postgresFixture) addHandoffChain(
	t *testing.T,
	ctx context.Context,
	work leasedRun,
) handoffChain {
	t.Helper()
	chain := fixture.base.AddHandoffChain(
		t,
		ctx,
		runtest.RunLease{LeaseID: work.leaseID, RunID: work.runID},
	)
	return handoffChain{
		outerRunID:          chain.OuterRunID,
		parentRunID:         chain.ParentRunID,
		outerWaitID:         chain.OuterWaitID,
		outerCheckpoint:     chain.OuterCheckpoint,
		outerResumeID:       chain.OuterResumeID,
		enclosingWaitID:     chain.EnclosingWaitID,
		enclosingCheckpoint: chain.EnclosingCheckpoint,
		enclosingResumeID:   chain.EnclosingResumeID,
		runtimeID:           chain.RuntimeID,
		mountID:             chain.MountID,
		versionID:           chain.VersionID,
	}
}

func (fixture postgresFixture) runPublicID(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var publicID string
	if err := fixture.pool.QueryRow(
		t.Context(),
		`SELECT public_id FROM runs WHERE id = $1`,
		runID,
	).Scan(&publicID); err != nil {
		t.Fatal(err)
	}
	return publicID
}

func mustExec(t *testing.T, ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	runtest.MustExec(t, ctx, executor, query, args...)
}
