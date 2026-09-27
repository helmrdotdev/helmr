package dispatch

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func queuedSharedRun(t *testing.T, f runtest.Fixture, work runtest.RunLease) ReadyRunCandidate {
	t.Helper()
	id := uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO runs(id,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,queue_origin_at,queue_score_at,max_active_duration_ms,retry_policy,trace_id,root_span_id)
 SELECT $2,org_id,project_id,environment_id,deployment_id,deployment_definition_id,entrypoint_kind,entrypoint_declared_id,cause_kind,computer_id,base_computer_disk_version_id,payload,queue_name,clock_timestamp(),clock_timestamp(),max_active_duration_ms,retry_policy,trace_id,root_span_id FROM runs WHERE id=$1`, work.RunID, id)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id) SELECT id,1,entrypoint_kind,computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, id)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(id), ExpectedRunRevision: 1}
}
func TestRunsAndCommandsSharePhysicalWriter(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	first, second := queuedSharedRun(t, f, work), queuedSharedRun(t, f, work)
	command := pendingSharedCommand(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET max_vm_slots=1 WHERE id=$1`, f.WorkerID)
	for _, candidate := range []ReadyRunCandidate{first, second} {
		p, err := a.PlaceReadyRun(t.Context(), candidate)
		if err != nil {
			t.Fatal(err)
		}
		if !p.LeaseCreated || p.Lease.WriterGeneration != 2 {
			t.Fatalf("placement=%+v", p)
		}
	}
	if p, err := a.PlaceComputerCommand(t.Context(), command); err != nil || !p.ProcessBound {
		t.Fatalf("Command=%+v err=%v", p, err)
	}
	var count int
	var generation int64
	var membership int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),min(writer_generation),min(membership_revision) FROM computer_instances WHERE computer_id=(SELECT computer_id FROM runs WHERE id=$1) AND reclaimed_at IS NULL`, work.RunID).Scan(&count, &generation, &membership); err != nil {
		t.Fatal(err)
	}
	if count != 1 || generation != 2 || membership != 3 {
		t.Fatalf("instances=%d generation=%d membership=%d", count, generation, membership)
	}
	if _, err := a.PlaceReadyRun(t.Context(), first); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("duplicate grant=%v", err)
	}
}
func TestRunWaitsForProgramPreparationAcknowledgement(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	// Start with an independently prepared Computer that has no resident Program.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now() WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
	command := pendingSharedCommand(t, f, work)
	initial, err := a.PlaceComputerCommand(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='ready',ready_at=clock_timestamp(),observed_desired_version=desired_version,mount_state='mounted',mounted_at=clock_timestamp() WHERE id=$1`, initial.ComputerInstanceID)
	candidate := queuedSharedRun(t, f, work)
	loading, err := a.PlaceReadyRun(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if loading.LeaseCreated {
		t.Fatal("Run granted before Program was loaded")
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: initial.ComputerInstanceID})
	if err != nil {
		t.Fatal(err)
	}
	if i.ProgramDeploymentID != pgvalue.UUID(f.DeploymentID) || i.DesiredVersion != 2 || i.ObservedDesiredVersion != 1 || i.WriterGeneration != 3 {
		t.Fatalf("Program preparation=%+v", i)
	}
	if p, err := a.PlaceReadyRun(t.Context(), candidate); err != nil || p.LeaseCreated {
		t.Fatalf("unacknowledged Program grant=%+v %v", p, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_desired_version=desired_version WHERE id=$1`, i.ID)
	p, err := a.PlaceReadyRun(t.Context(), candidate)
	if err != nil || !p.LeaseCreated || p.Lease.WriterGeneration != 3 {
		t.Fatalf("acknowledged Program grant=%+v %v", p, err)
	}
}

func TestFreshRunQueueLimitRetainsSharedInstance(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	candidate := queuedSharedRun(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET queue_concurrency_limit=1 WHERE id=$1`, candidate.RunID)
	if _, err := a.PlaceReadyRun(t.Context(), candidate); !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("queue limit=%v", err)
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='queued' AND r.current_run_lease_id IS NULL AND i.desired_state='ready' AND i.reclaimed_at IS NULL AND i.membership_revision=0 FROM runs r JOIN computer_instances i ON i.computer_id=r.computer_id AND i.reclaimed_at IS NULL WHERE r.id=$1`, candidate.RunID).Scan(&retained); err != nil || !retained {
		t.Fatalf("queued member or Instance changed=%v %v", !retained, err)
	}
	// Another admission can use the same resident writer after capacity is available.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET queue_concurrency_limit=2 WHERE id=$1`, candidate.RunID)
	placed, err := a.PlaceReadyRun(t.Context(), candidate)
	if err != nil || !placed.LeaseCreated {
		t.Fatalf("admission after capacity available=%+v %v", placed, err)
	}
}
