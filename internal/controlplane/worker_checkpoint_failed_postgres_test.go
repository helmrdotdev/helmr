package controlplane

import (
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func checkpointFailureFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workerapi.CheckpointFailedRequest, workerHTTPClient, http.Handler) {
	t.Helper()
	f := runtest.New(t)
	run := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='waiting' WHERE id=$1`, run.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, run.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_waits(id,environment_id,run_id,computer_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id) SELECT $2,environment_id,id,computer_id,'timer',now()+interval '1 hour',revision,1,$3 FROM runs WHERE id=$1`, run.RunID, uuid.NewV7(), run.LeaseID)
	capture := computer.Capture{CheckpointID: uuid.NewV7(), EnvironmentID: f.EnvironmentID}
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,i.membership_revision,i.desired_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, run.LeaseID).Scan(&capture.InstanceID, &capture.WriterGeneration, &capture.MembershipRevision, &capture.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		_, err := computer.BeginCapture(t.Context(), tx, capture)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	handler := newPostgresServer(t, f.Pool)
	worker := newWorkerHTTPClient(t, handler, f.Pool, f.WorkerID)
	return f, run, workerapi.CheckpointFailedRequest{ComputerInstanceID: capture.InstanceID.String(), WorkerEpoch: 1, DesiredVersion: capture.DesiredVersion + 1, CheckpointID: capture.CheckpointID.String(), Error: "snapshot failed"}, worker, handler
}

// otherWorkerHost seeds a second active worker host in the fixture host's
// Group and Pool at the same epoch, with its own service.
func otherWorkerHost(t *testing.T, f runtest.Fixture) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts (id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,vm_platform_id,
 epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,
 max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,observed_at,epoch_started_at,activated_at)
 SELECT $1,$4,worker_group_id,worker_pool_id,status,current_epoch,$2,vm_platform_id,
 epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,
 max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,now(),now(),now() FROM worker_hosts WHERE id=$3`, id, uuid.NewV7(), f.WorkerID, id.String())
	return id
}

func TestWorkerCheckpointFailureRequestsSourceExclusion(t *testing.T) {
	f, run, request, worker, _ := checkpointFailureFixture(t)
	for range 2 {
		var receipt workerapi.ComputerCheckpointResponse
		worker.post(t, checkpointFailedPath, request, http.StatusOK, &receipt)
		if receipt.ComputerInstanceID != request.ComputerInstanceID || receipt.WorkerEpoch != request.WorkerEpoch || receipt.DesiredVersion != request.DesiredVersion || receipt.CheckpointID != request.CheckpointID {
			t.Fatalf("receipt=%+v", receipt)
		}
	}
	var preserved bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status='waiting' AND l.process_reconciled_at IS NULL AND i.reclaimed_at IS NULL AND i.desired_state='closed' AND i.desired_version=$2 FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computer_instances i ON i.id=l.computer_instance_id WHERE r.id=$1`, run.RunID, request.DesiredVersion+1).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("source retained=%v err=%v", preserved, err)
	}
	request.Error = "different failure"
	worker.post(t, checkpointFailedPath, request, http.StatusConflict, nil)
}

// An authenticated worker host cannot report the failure of another host's
// capture: the original request from a different host is a changed source and
// leaves the capture untouched for its own host.
func TestWorkerCheckpointFailureRejectsAnotherHost(t *testing.T) {
	f, _, request, worker, handler := checkpointFailureFixture(t)
	other := newWorkerHTTPClient(t, handler, f.Pool, otherWorkerHost(t, f))
	other.post(t, checkpointFailedPath, request, http.StatusConflict, nil)
	var unchanged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.status='creating' AND c.failed_request_fingerprint IS NULL AND i.desired_state='ready' AND i.admission_state='checkpointing' AND i.desired_version=$2 FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, request.CheckpointID, request.DesiredVersion).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("another host changed the capture=%v err=%v", !unchanged, err)
	}
	worker.post(t, checkpointFailedPath, request, http.StatusOK, nil)
}

func TestWorkerCheckpointFailureRejectsInvalidSource(t *testing.T) {
	_, _, request, worker, _ := checkpointFailureFixture(t)
	wrongSource := request
	wrongSource.ComputerInstanceID = uuid.NewV7().String()
	worker.post(t, checkpointFailedPath, wrongSource, http.StatusConflict, nil)
	malformed := request
	malformed.CheckpointID = "not-a-uuid"
	worker.post(t, checkpointFailedPath, malformed, http.StatusBadRequest, nil)
	unversioned := request
	unversioned.DesiredVersion = 0
	worker.post(t, checkpointFailedPath, unversioned, http.StatusBadRequest, nil)
	request.Error = " "
	worker.post(t, checkpointFailedPath, request, http.StatusBadRequest, nil)
}
