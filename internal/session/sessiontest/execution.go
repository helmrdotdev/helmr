package sessiontest

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// Execution is an Actor execution on a run test database. Its Session was
// converted from an assigned Task Run lease of the fixture worker: the
// Actor's idle timeout is one second, its queue admits eight Runs, and its
// Session and boot Run start with no committed input. The lease is not yet
// claimed.
type Execution struct {
	runtest.Fixture
	// Worker is the principal of the fixture worker that runs the lease.
	Worker workergroup.HostPrincipal
	// ComputerID is the Session's Computer and RootID its base disk version.
	ComputerID, RootID uuid.UUID
	SessionID, RunID   uuid.UUID
	LeaseID            uuid.UUID
	// Claim is the worker's claim of the lease, once ClaimLease claimed it.
	Claim run.Claim
}

// NewExecution builds a run test database whose deployment's default queue
// admits eight Runs and whose worker pool and host have guest ephemeral disk
// capacity, and an Actor execution on it.
func NewExecution(t *testing.T) *Execution {
	t.Helper()
	base := runtest.New(t)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE deployments SET queue_config='{"formatVersion":0,"queues":[{"concurrencyLimit":8,"name":"default"},{"name":"priority"}]}' WHERE id=$1`, base.DeploymentID)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE worker_pools SET per_vm_guest_ephemeral_disk_bytes=34359738368,capacity_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, base.WorkerPoolID)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE worker_hosts SET per_vm_guest_ephemeral_disk_bytes=34359738368,epoch_guest_ephemeral_disk_bytes=274877906944 WHERE id=$1`, base.WorkerID)
	return ExecutionOn(t, base)
}

// ExecutionOn adds another Actor execution to the run test database.
func ExecutionOn(t *testing.T, base runtest.Fixture) *Execution {
	t.Helper()
	work := base.AddRunLease(t, "assigned", time.Now())
	sid := base.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE runs SET queue_concurrency_limit=8 WHERE id=$1`, work.RunID)
	manifest, digest, err := definition.CanonicalManifestAndDigest([]byte(`{"idleTimeoutMs":1000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=(SELECT deployment_definition_id FROM sessions WHERE id=$1)`, sid, manifest, digest[:])
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE sessions SET committed_input_sequence=0,next_input_sequence=1 WHERE id=$1`, sid)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE runs SET session_input_start_sequence=0,session_input_high_watermark=0 WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), base.Pool, `UPDATE run_attempts SET session_input_start_sequence=0 WHERE run_id=$1`, work.RunID)
	f := &Execution{Fixture: base, SessionID: sid, RunID: work.RunID, LeaseID: work.LeaseID,
		Worker: workergroup.HostPrincipal{HostID: base.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}}
	if err := base.Pool.QueryRow(t.Context(), `SELECT computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, work.RunID).Scan(&f.ComputerID, &f.RootID); err != nil {
		t.Fatal(err)
	}
	return f
}

// ClaimLease claims the lease for the fixture worker.
func (f *Execution) ClaimLease(t *testing.T) {
	t.Helper()
	var err error
	f.Claim, err = run.ClaimLease(t.Context(), f.Pool, f.ExecutionFence(1))
	if err != nil {
		t.Fatal(err)
	}
}

// ClaimAndStart claims the lease, starts it and enters the Actor entrypoint.
func (f *Execution) ClaimAndStart(t *testing.T) {
	t.Helper()
	f.ClaimLease(t)
	f.StartLease(t, "actor", "test-actor")
}

// StartLease starts the claimed lease and enters its entrypoint.
func (f *Execution) StartLease(t *testing.T, kind, declaredID string) {
	t.Helper()
	fence := f.Fence()
	if err := run.StartLease(t.Context(), f.Pool, fence); err != nil {
		t.Fatal(err)
	}
	if err := run.EnterEntrypoint(t.Context(), f.Pool, fence, kind, declaredID); err != nil {
		t.Fatal(err)
	}
}

// ExecutionFence is the fixture worker's fence on its lease at the sequence.
func (f *Execution) ExecutionFence(sequence int64) run.ExecutionFence {
	return run.ExecutionFence{LeaseID: pgvalue.UUID(f.LeaseID), LeaseSequence: sequence, WorkerGroupID: pgvalue.UUID(f.Worker.GroupID), WorkerHostID: pgvalue.UUID(f.Worker.HostID), WorkerEpoch: f.Worker.Epoch, GroupClaimVersion: f.Worker.GroupClaimVersion, HostClaimVersion: f.Worker.HostClaimVersion}
}

// Fence is the fixture worker's fence on its claimed lease.
func (f *Execution) Fence() run.ExecutionFence {
	return f.ExecutionFence(f.Claim.Lease().LeaseSequence)
}
