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

func checkpointFailureFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workerapi.CheckpointFailedRequest, workerHTTPClient) {
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
	worker := newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.WorkerID)
	return f, run, workerapi.CheckpointFailedRequest{ComputerInstanceID: capture.InstanceID.String(), WorkerEpoch: 1, DesiredVersion: capture.DesiredVersion + 1, CheckpointID: capture.CheckpointID.String(), Error: "snapshot failed"}, worker
}

func TestWorkerCheckpointFailureRequestsSourceExclusion(t *testing.T) {
	f, run, request, worker := checkpointFailureFixture(t)
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

func TestWorkerCheckpointFailureRejectsInvalidSource(t *testing.T) {
	_, _, request, worker := checkpointFailureFixture(t)
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
