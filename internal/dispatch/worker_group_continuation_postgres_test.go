package dispatch_test

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

const pauseWorkerGroup = `UPDATE worker_groups SET status='paused',claim_version=claim_version+1 WHERE id=$1`

// A paused Worker Group stops admission only; resident Computers still capture.
func TestComputerCaptureContinuesOnPausedGroup(t *testing.T) {
	f, _, _, request := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, pauseWorkerGroup, runtest.WorkerGroupID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	cp, err := computer.BeginCapture(t.Context(), tx, request)
	if err != nil {
		t.Fatalf("capture on paused Group: %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	params := db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: pgvalue.UUID(request.InstanceID), EnvironmentID: pgvalue.UUID(request.EnvironmentID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: request.DesiredVersion + 1, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds}
	if got, err := db.New(f.Pool).GetComputerInstanceCaptureCheckpoint(t.Context(), params); err != nil || got.ID != cp.ID {
		t.Fatalf("capture discovery on paused Group: %v %v", got.ID, err)
	}
}

func TestComputerCheckpointRegistrationContinuesOnPausedGroup(t *testing.T) {
	f, ref, manifest := computertest.RegisteredCapture(t, false)
	dbtest.MustExec(t, t.Context(), f.Pool, pauseWorkerGroup, runtest.WorkerGroupID)
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatalf("registration on paused Group: %v", err)
	}
}

// nonAdmittingSupply stops admission while admitted work continues.
var nonAdmittingSupply = []struct{ name, sql string }{
	{"paused Group", `UPDATE worker_groups SET status='paused',claim_version=claim_version+1 WHERE id=$1`},
	{"draining Group", `UPDATE worker_groups SET status='draining',primary_pool_id=NULL,claim_version=claim_version+1 WHERE id=$1`},
	{"paused Host", `UPDATE worker_hosts SET run_paused_reason='startup_recovery_leak',vm_paused_reason='runtime_health' WHERE worker_group_id=$1`},
	{"draining Pool", `WITH g AS (UPDATE worker_groups SET primary_pool_id=NULL WHERE id=$1 RETURNING id) UPDATE worker_pools SET status='draining' WHERE worker_group_id=(SELECT id FROM g)`},
	{"draining Host", `WITH h AS (UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp() WHERE worker_group_id=$1 RETURNING id) UPDATE computer_instances SET admission_state='draining' WHERE worker_host_id IN (SELECT id FROM h) AND admission_state='open'`},
}

type restoreHarness struct {
	f         runtest.Fixture
	authority *dispatch.Authority
	fence     computer.InstanceRef
	cp        db.ComputerCheckpoint
	grants    []dispatch.ComputerRestoreGrant
}

func (h *restoreHarness) commit(t *testing.T) (db.ComputerCheckpoint, error) {
	t.Helper()
	tx, err := h.f.Pool.Begin(t.Context())
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	defer tx.Rollback(t.Context())
	cp, err := h.authority.CommitComputerRestore(t.Context(), tx, h.fence)
	if err == nil {
		err = tx.Commit(t.Context())
	}
	return cp, err
}

func (h *restoreHarness) acknowledge(t *testing.T) (db.ComputerInstance, error) {
	t.Helper()
	tx, err := h.f.Pool.Begin(t.Context())
	if err != nil {
		return db.ComputerInstance{}, err
	}
	defer tx.Rollback(t.Context())
	i, err := dispatch.AcknowledgeComputerRestore(t.Context(), tx, h.fence, h.cp.ID, h.cp.WriterGeneration+1, h.grants)
	if err == nil {
		err = tx.Commit(t.Context())
	}
	return i, err
}

func (h *restoreHarness) admission(t *testing.T) string {
	t.Helper()
	var state string
	if err := h.f.Pool.QueryRow(t.Context(), `SELECT admission_state FROM computer_instances WHERE id=$1`, h.fence.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// committedRestore commits a restore whose response the Worker then loses.
func committedRestore(t *testing.T) *restoreHarness {
	t.Helper()
	f, authority, fence := dispatchtest.Restore(t, false)
	h := &restoreHarness{f: f, authority: authority, fence: fence}
	cp, err := h.commit(t)
	if err != nil {
		t.Fatal(err)
	}
	h.cp, h.grants = cp, installedRestoreGrants(t, f, fence)
	return h
}

// Receipt replay for an opened restore continues on non-admitting supply.
func TestComputerRestoreReceiptReplayOnNonAdmittingSupply(t *testing.T) {
	for _, supply := range nonAdmittingSupply {
		t.Run(supply.name, func(t *testing.T) {
			h := committedRestore(t)
			if i, err := h.acknowledge(t); err != nil || i.AdmissionState != "open" {
				t.Fatalf("receipt: %v %v", i.AdmissionState, err)
			}
			dbtest.MustExec(t, t.Context(), h.f.Pool, supply.sql, runtest.WorkerGroupID)
			// Host drain moves an open Instance to draining admission.
			if i, err := h.acknowledge(t); err != nil || i.AdmissionState != h.admission(t) {
				t.Fatalf("receipt replay: %v %v", i.AdmissionState, err)
			}
			if cp, err := h.commit(t); err != nil || cp.ID != h.cp.ID {
				t.Fatalf("restore replay: %v %v", cp.ID, err)
			}
		})
	}
}

// A committed restore whose response was lost can be inspected on
// non-admitting supply, but its first activation starts Runs and is rejected.
func TestComputerRestoreCommittedReceiptBeforeActivationOnNonAdmittingSupply(t *testing.T) {
	for _, supply := range nonAdmittingSupply {
		t.Run(supply.name, func(t *testing.T) {
			h := committedRestore(t)
			dbtest.MustExec(t, t.Context(), h.f.Pool, supply.sql, runtest.WorkerGroupID)
			if cp, err := h.commit(t); err != nil || cp.ID != h.cp.ID {
				t.Fatalf("committed receipt inspection: %v %v", cp.ID, err)
			}
			if _, err := h.acknowledge(t); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("first activation: %v", err)
			}
			if state := h.admission(t); state != "restoring" {
				t.Fatalf("rejected activation changed admission to %s", state)
			}
		})
	}
}

// A restore that has not committed is a new start and needs admitting supply,
// including the readiness recheck on the restoring Instance.
func TestComputerRestoreFirstCommitRequiresAdmittingSupply(t *testing.T) {
	for _, supply := range nonAdmittingSupply {
		t.Run(supply.name, func(t *testing.T) {
			f, authority, fence := dispatchtest.Restore(t, false)
			h := &restoreHarness{f: f, authority: authority, fence: fence}
			dbtest.MustExec(t, t.Context(), f.Pool, supply.sql, runtest.WorkerGroupID)
			if _, err := h.commit(t); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("first commit: %v", err)
			}
			i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(fence.ID)})
			if err != nil {
				t.Fatal(err)
			}
			readiness := computer.Readiness{Observation: computer.Observation{Instance: fence, ExpectedObservedVersion: i.ObservedVersion}, VCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest}
			if _, err = computer.RecordInstanceReady(t.Context(), f.Pool, readiness); !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("restoring readiness: %v", err)
			}
		})
	}
}

// Restored Runs on an opened Instance stay discoverable and claimable after
// the Group pauses.
func TestRestoredExecutionContinuesOnPausedGroup(t *testing.T) {
	h := committedRestore(t)
	if _, err := h.acknowledge(t); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), h.f.Pool, pauseWorkerGroup, runtest.WorkerGroupID)
	discovered, err := db.New(h.f.Pool).DiscoverWorkerRunLeaseWork(t.Context(), db.DiscoverWorkerRunLeaseWorkParams{WorkerHostID: pgvalue.UUID(h.f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1, RowLimit: 10})
	if err != nil || len(discovered) != len(h.grants) {
		t.Fatalf("restored discovery on paused Group: %v %v", discovered, err)
	}
	for _, g := range h.grants {
		fence := run.ExecutionFence{LeaseID: g.LeaseID, LeaseSequence: g.LeaseSequence, WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(h.f.WorkerID), WorkerEpoch: 1}
		if err = h.f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, h.f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
			t.Fatal(err)
		}
		tx, err := h.f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = run.ClaimRestoredExecution(t.Context(), tx, fence)
		if err == nil {
			err = tx.Commit(t.Context())
		}
		tx.Rollback(t.Context())
		if err != nil {
			t.Fatalf("restored claim on paused Group: %v", err)
		}
	}
}

func TestComputerCaptureWorkerFreshRejectsUnobservedWorker(t *testing.T) {
	f, _, _, _ := computertest.Capture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=NULL WHERE id=$1`, f.WorkerID)
	fresh, err := db.New(f.Pool).GetComputerCaptureWorkerFresh(t.Context(), db.GetComputerCaptureWorkerFreshParams{ID: pgvalue.UUID(f.WorkerID), WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
	if err != nil || fresh {
		t.Fatalf("unobserved Worker fresh=%v err=%v", fresh, err)
	}
}
